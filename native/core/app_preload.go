package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type nativePreloadKey struct{}

const nativePreloadLimit = 4 << 20

// nativePreloadBudget 预加载的时间预算（两条线路共享，不因切线路而翻倍）。
const nativePreloadBudget = 6 * time.Second

// nativePreloadFirstByteLimit 首字节（首个分片/播放列表）超过这个时长即判定
// 「该线路慢/不通」，切下一条再试。判定放在预加载窗口里，是为了让试错不占用
// 播放窗口——切线路本身是本地操作（nativeNextPlayback 只递增 index），
// 不产生新的解析请求。
const nativePreloadFirstByteLimit = 1500 * time.Millisecond

// nativePrefetchResult 一次预取的结果。
type nativePrefetchResult struct {
	Bytes       int64
	FirstByteMS int64
	OK          bool
}

func (engine *nativeEngine) nativePreload(ctx context.Context, input nativeInput) (nativePlan, error) {
	ctx = context.WithValue(ctx, backgroundCatalogKey{}, true)
	ctx = context.WithValue(ctx, nativePreloadKey{}, true)
	plan, err := engine.nativeResolve(ctx, input)
	if err != nil || plan.Local {
		return plan, err
	}
	engine.mu.Lock()
	choice, exists := engine.playbacks[plan.Session]
	stream := engine.stream
	engine.mu.Unlock()
	if exists && stream != nil && choice.streamSession != "" {
		deadline := time.Now().Add(nativePreloadBudget)
		plan, _ = engine.nativePreloadWarm(ctx, stream, plan, choice, input.Quality, deadline)
	}
	if err := ctx.Err(); err != nil {
		engine.nativeReleasePlayback(plan.Session)
		return nativePlan{}, err
	}
	return plan, nil
}

// nativePreloadWarm 预取并按可用性挑选线路。
//
// 首选线路若在预算内拿不到首字节（或完全失败），就本地切到下一条线路再探一次；
// 两次都失败时保留**预取字节更多**的那一版（已有的预取不浪费）。
// 全程串行、共享同一时间预算，因此请求总量与旧实现相当。
func (engine *nativeEngine) nativePreloadWarm(
	ctx context.Context,
	stream *nativeStreamServer,
	plan nativePlan,
	choice nativePlaybackChoice,
	quality int,
	deadline time.Time,
) (nativePlan, nativePlaybackChoice) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return plan, choice
	}
	warming, cancel := context.WithTimeout(ctx, remaining)
	result := stream.nativePrefetch(warming, choice.streamSession, plan.URL, quality)
	cancel()
	plan.PrefetchedBytes = result.Bytes
	plan.Verified = result.OK && result.FirstByteMS > 0 && result.FirstByteMS <= nativePreloadFirstByteLimit.Milliseconds()
	// 【回流】探活发现「解析得出来、但地址播不通」——这类失败只有播放层能观测到，
	// 若不回流，下次解析还会优先选这条线路。归为 network（取不到首字节）。
	if !plan.Verified && choice.route != "" {
		hongguoRoutesHealth.noteFail(choice.route, time.Now(), hongguoFailNetwork)
	}
	if plan.Verified || choice.index+1 >= len(choice.media) {
		return plan, choice
	}
	if time.Until(deadline) <= 0 {
		return plan, choice
	}
	// 下一条线路：本地切换，零解析请求。
	previousSession := plan.Session
	next, err := engine.nativeNextPlayback(ctx, previousSession)
	if err != nil {
		return plan, choice
	}
	engine.nativeReleasePlayback(previousSession)
	engine.mu.Lock()
	nextChoice, exists := engine.playbacks[next.Session]
	engine.mu.Unlock()
	if !exists || nextChoice.streamSession == "" {
		return plan, choice
	}
	warming, cancel = context.WithTimeout(ctx, time.Until(deadline))
	nextResult := stream.nativePrefetch(warming, nextChoice.streamSession, next.URL, quality)
	cancel()
	next.PrefetchedBytes = nextResult.Bytes
	next.Verified = nextResult.OK && nextResult.FirstByteMS > 0 && nextResult.FirstByteMS <= nativePreloadFirstByteLimit.Milliseconds()
	if !next.Verified && !nextResult.OK && result.OK {
		// 新的更差：回收，保留原线路（已预取的字节仍然有效）。
		engine.nativeReleasePlayback(next.Session)
		return plan, choice
	}
	if !next.Verified && result.OK && nextResult.Bytes <= result.Bytes {
		engine.nativeReleasePlayback(next.Session)
		return plan, choice
	}
	return next, nextChoice
}

func (stream *nativeStreamServer) nativePrefetch(ctx context.Context, token, entry string, quality int) nativePrefetchResult {
	stream.mu.Lock()
	session := stream.sessions[token]
	stream.mu.Unlock()
	if session == nil {
		return nativePrefetchResult{}
	}
	ctx, cancel := context.WithCancel(providerMediaContext(ctx, session.credentials))
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	remaining, requests := int64(nativePreloadLimit), 0
	visited := map[string]bool{}
	started := time.Now()
	var firstByte time.Duration
	var warm func(string, int) bool
	warm = func(local string, depth int) bool {
		if ctx.Err() != nil || remaining <= 0 || requests >= 8 || depth > 2 || visited[local] {
			return false
		}
		visited[local] = true
		parsed, err := url.Parse(local)
		if err != nil || !strings.HasPrefix(local, stream.address+"/"+token+"/") {
			return false
		}
		id := strings.TrimPrefix(parsed.Path, "/"+token+"/")
		session.mu.Lock()
		asset, exists := session.assets[id]
		session.mu.Unlock()
		if !exists {
			return false
		}
		playlist := strings.Contains(asset.contentType, "mpegurl")
		if len(asset.data) == 0 {
			requests++
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.address, nil)
			if err != nil {
				return false
			}
			request.Header.Set("Referer", session.referer)
			if !playlist {
				request.Header.Set("Range", fmt.Sprintf("bytes=0-%d", remaining-1))
			}
			response, err := stream.downloader.doMediaRequest(request)
			if err != nil {
				return false
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, remaining+1))
			response.Body.Close()
			if readErr != nil || len(body) == 0 || int64(len(body)) > remaining ||
				(response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent) {
				return false
			}
			if response.Request != nil && response.Request.URL != nil {
				asset.address = response.Request.URL.String()
			}
			asset.contentType = response.Header.Get("Content-Type")
			asset.etag, asset.modified = response.Header.Get("ETag"), response.Header.Get("Last-Modified")
			playlist = playlist || strings.Contains(strings.ToLower(asset.contentType), "mpegurl") || bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U"))
			asset.data = body
			asset.total = int64(len(body))
			if response.StatusCode == http.StatusPartialContent {
				start, end, total, valid := nativeContentRange(response.Header.Get("Content-Range"))
				if !valid || start != 0 || end+1 != int64(len(body)) {
					return false
				}
				asset.total = total
			}
			if asset.total > int64(len(body)) && asset.modified == "" && (asset.etag == "" || strings.HasPrefix(asset.etag, "W/")) {
				return false
			}
			if playlist {
				asset.contentType = "application/vnd.apple.mpegurl"
			}
			remaining -= int64(len(body))
			if firstByte <= 0 {
				// 首个分片/播放列表到手的时间，作为该线路「快不快」的判据。
				firstByte = time.Since(started)
			}
			session.mu.Lock()
			session.assets[id] = asset
			session.mu.Unlock()
		}
		if !playlist {
			return true
		}
		lines, _, master := nativeHLSSelect(strings.Split(string(asset.data), "\n"), quality)
		rewritten, err := stream.nativeRewrite(token, session, strings.Join(lines, "\n"), asset.address)
		if err != nil {
			return false
		}
		segments := 0
		seconds := 0.0
		for _, raw := range strings.Split(rewritten, "\n") {
			line := strings.TrimSpace(raw)
			if strings.HasPrefix(line, "#EXT-X-BYTERANGE:") {
				break
			}
			if strings.HasPrefix(line, "#EXTINF:") {
				value := strings.SplitN(strings.TrimPrefix(line, "#EXTINF:"), ",", 2)[0]
				duration, _ := strconv.ParseFloat(value, 64)
				seconds += duration
			}
			if strings.HasPrefix(line, "#EXT-X-KEY:") || strings.HasPrefix(line, "#EXT-X-MAP:") {
				for _, match := range nativePlaylistURI.FindAllStringSubmatch(line, -1) {
					if !strings.HasPrefix(match[1], "data:") && !warm(match[1], depth+1) {
						return false
					}
				}
			}
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !warm(line, depth+1) {
				break
			}
			segments++
			if master || segments >= 2 || seconds >= 8 {
				break
			}
		}
		return true
	}
	warm(entry, 0)
	fetched := nativePreloadLimit - remaining
	// 向上取整到毫秒：本机代理下首字节可能不足 1ms，取 0 会被误判成"没测到"。
	firstMS := int64((firstByte + time.Millisecond - 1) / time.Millisecond)
	return nativePrefetchResult{
		Bytes:       fetched,
		FirstByteMS: firstMS,
		OK:          fetched > 0,
	}
}

var nativeContentRangePattern = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

func nativeContentRange(value string) (int64, int64, int64, bool) {
	match := nativeContentRangePattern.FindStringSubmatch(value)
	if match == nil {
		return 0, 0, 0, false
	}
	start, first := strconv.ParseInt(match[1], 10, 64)
	end, second := strconv.ParseInt(match[2], 10, 64)
	total, third := strconv.ParseInt(match[3], 10, 64)
	return start, end, total, first == nil && second == nil && third == nil && start >= 0 && end >= start && total > end
}

func (stream *nativeStreamServer) nativeServePrefix(ctx context.Context, writer http.ResponseWriter, request *http.Request, session *nativeStreamSession, asset nativeStreamAsset) bool {
	start, end := int64(0), asset.total-1
	ranged := request.Header.Get("Range") != ""
	if ranged {
		value := request.Header.Get("Range")
		if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
			return false
		}
		parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
		if len(parts) != 2 {
			return false
		}
		var err error
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 || start >= int64(len(asset.data)) {
			return false
		}
		if parts[1] != "" {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || end < start {
				return false
			}
			end = min(end, asset.total-1)
		}
	}
	validator := asset.etag
	if validator == "" || strings.HasPrefix(validator, "W/") {
		validator = asset.modified
	}
	if value := request.Header.Get("If-Range"); value != "" && value != validator {
		return false
	}
	var response *http.Response
	if end >= int64(len(asset.data)) && request.Method != http.MethodHead {
		upstream, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.address, nil)
		if err != nil || validator == "" {
			return false
		}
		upstream.Header.Set("Referer", session.referer)
		upstream.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", len(asset.data), end))
		upstream.Header.Set("If-Range", validator)
		response, err = stream.nativeRequest(upstream)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		a, b, total, valid := nativeContentRange(response.Header.Get("Content-Range"))
		if response.StatusCode != http.StatusPartialContent || !valid || a != int64(len(asset.data)) || b != end || total != asset.total {
			session.invalidate(asset.address)
			return false
		}
		if validator == asset.etag && response.Header.Get("ETag") != validator ||
			validator == asset.modified && response.Header.Get("Last-Modified") != validator {
			session.invalidate(asset.address)
			return false
		}
	}
	writer.Header().Set("Content-Type", asset.contentType)
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if asset.etag != "" {
		writer.Header().Set("ETag", asset.etag)
	}
	if asset.modified != "" {
		writer.Header().Set("Last-Modified", asset.modified)
	}
	if ranged {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, asset.total))
		writer.WriteHeader(http.StatusPartialContent)
	}
	if request.Method == http.MethodHead {
		return true
	}
	_, err := writer.Write(asset.data[start:min(end+1, int64(len(asset.data)))])
	if err == nil && response != nil {
		_, _ = io.CopyN(writer, response.Body, end-int64(len(asset.data))+1)
	}
	return true
}

func (session *nativeStreamSession) invalidate(address string) {
	session.mu.Lock()
	defer session.mu.Unlock()
	for id, asset := range session.assets {
		if asset.address == address {
			asset.data, asset.total, asset.etag, asset.modified = nil, 0, "", ""
			session.assets[id] = asset
		}
	}
}
