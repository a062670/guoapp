package core

// MacCMS XML 采集接口（TVBox type=2）解析。
//
// 接口地址形如 https://host/api.php/provide/vod/at/xml/，与 JSON 版是同一套
// 苹果CMS 数据源，只是序列化成了 XML：
//
//	<rss version="5.1">
//	  <list page="1" pagecount="7996" pagesize="20" recordcount="159903">
//	    <video>
//	      <id>161963</id><tid>13</tid>
//	      <name><![CDATA[嫁金钗]]></name><type>国产剧</type>
//	      <pic>…</pic><lang>汉语普通话</lang><area>中国大陆</area><year>2026</year>
//	      <note><![CDATA[更新至第10集]]></note>
//	      <actor><![CDATA[田曦薇,闫桉]]></actor><director><![CDATA[杨龙]]></director>
//	      <des><![CDATA[<p>简介…</p>]]></des>
//	      <dl>
//	        <dd flag="liangzi"><![CDATA[第01集$https://…m3u8#第02集$…]]></dd>
//	      </dl>
//	    </video>
//	  </list>
//	  <class><ty id="13">国产剧</ty>…</class>
//	</rss>
//
// 参数与 JSON 版一致：?ac=list 取分类（同响应也带一页简版列表）、
// ?ac=detail&pg=N&t=分类 取列表、?ac=detail&ids=ID 取详情、wd= 搜索。
// 多个播放源用多个 <dd flag="源名"> 表示，分集间隔符仍是 # 与 $，
// 因此分集解析复用 libretvChaptersFromGroups。

import (
	"context"
	"encoding/xml"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// errLibreTVNotXML 接口返回的不是 XML —— 用于触发 JSON 格式回退。
var errLibreTVNotXML = errors.New("片源接口返回的不是 XML，可能不是苹果 CMS XML 采集接口")

// macCMSXMLDocument 苹果CMS XML 采集接口响应信封。
type macCMSXMLDocument struct {
	XMLName xml.Name `xml:"rss"`
	List    struct {
		Page        string           `xml:"page,attr"`
		PageCount   string           `xml:"pagecount,attr"`
		PageSize    string           `xml:"pagesize,attr"`
		RecordCount string           `xml:"recordcount,attr"`
		Videos      []macCMSXMLVideo `xml:"video"`
	} `xml:"list"`
	Class struct {
		Types []struct {
			ID   string `xml:"id,attr"`
			Name string `xml:",chardata"`
		} `xml:"ty"`
	} `xml:"class"`
}

// macCMSXMLVideo 单条影片。列表接口只填简版字段，详情接口才会带
// pic / actor / director / des / dl。
type macCMSXMLVideo struct {
	Last     string `xml:"last"`
	ID       string `xml:"id"`
	TypeID   string `xml:"tid"`
	Name     string `xml:"name"`
	Type     string `xml:"type"`
	Pic      string `xml:"pic"`
	Lang     string `xml:"lang"`
	Area     string `xml:"area"`
	Year     string `xml:"year"`
	State    string `xml:"state"`
	Note     string `xml:"note"`
	Actor    string `xml:"actor"`
	Director string `xml:"director"`
	Score    string `xml:"score"`
	Tag      string `xml:"tag"`
	Content  string `xml:"content"`
	Des      string `xml:"des"`
	PlayList []struct {
		Flag string `xml:"flag,attr"`
		URL  string `xml:",chardata"`
	} `xml:"dl>dd"`
}

// row 归一化成与 JSON 接口共用的列表行。
func (video macCMSXMLVideo) row() macCMSListRow {
	return macCMSListRow{
		ID:        strings.TrimSpace(video.ID),
		Name:      strings.TrimSpace(video.Name),
		Pic:       strings.TrimSpace(video.Pic),
		TypeName:  strings.TrimSpace(video.Type),
		Remarks:   strings.TrimSpace(video.Note),
		Year:      strings.TrimSpace(video.Year),
		Area:      strings.TrimSpace(video.Area),
		Lang:      strings.TrimSpace(video.Lang),
		Hits:      "", // XML 接口不提供播放量
		Score:     libretvScoreText(strings.TrimSpace(video.Score)),
		Actor:     strings.TrimSpace(video.Actor),
		Director:  strings.TrimSpace(video.Director),
		UpdatedAt: strings.TrimSpace(video.Last),
		Tag:       strings.TrimSpace(video.Tag),
	}
}

// description 详情简介：不同站点用 des 或 content，取首个非空的。
func (video macCMSXMLVideo) description() string {
	for _, raw := range []string{video.Des, video.Content} {
		if text := libretvHTMLText(raw); text != "" {
			return text
		}
	}
	return ""
}

// playGroups 把多个 <dd flag> 拆成与 JSON 接口同构的两个平行数组。
func (video macCMSXMLVideo) playGroups() (playFrom, playGroups []string) {
	for _, entry := range video.PlayList {
		group := strings.TrimSpace(entry.URL)
		if group == "" {
			continue
		}
		playFrom = append(playFrom, strings.TrimSpace(entry.Flag))
		playGroups = append(playGroups, group)
	}
	return playFrom, playGroups
}

// libretvXMLAPI 请求一次 XML 接口并按参数解码。
func (d *Downloader) libretvXMLAPI(ctx context.Context, site libretvSite, params map[string]string) (*macCMSXMLDocument, error) {
	address, err := url.Parse(site.API)
	if err != nil {
		return nil, errors.New("片源接口地址无效")
	}
	query := address.Query()
	for key, value := range params {
		query.Set(key, value)
	}
	address.RawQuery = query.Encode()
	referer := address.Scheme + "://" + address.Host + "/"
	body, err := d.fetchProviderText(ctx, address.String(), referer)
	if err != nil {
		return nil, err
	}
	// 去掉 BOM 与首尾空白后再判断：部分站点会在 XML 前输出空行。
	trimmed := strings.TrimSpace(strings.TrimPrefix(body, "\ufeff"))
	if !strings.HasPrefix(trimmed, "<") {
		return nil, errLibreTVNotXML
	}
	var document macCMSXMLDocument
	if xml.Unmarshal([]byte(trimmed), &document) != nil {
		return nil, errors.New("片源接口返回的不是有效 XML，可能不是苹果 CMS XML 采集接口")
	}
	if document.XMLName.Local != "rss" {
		return nil, errLibreTVNotXML
	}
	return &document, nil
}

// fetchLibreTVCatalogPageXML 拉取一页 XML 列表（ac=detail）。
func (d *Downloader) fetchLibreTVCatalogPageXML(ctx context.Context, site libretvSite, page int, category, query string) ([]Drama, bool, error) {
	params := map[string]string{"ac": "detail"}
	requestedPage := page
	if page > 1 {
		params["pg"] = strconv.Itoa(page)
	}
	if category != "" {
		params["t"] = category
	}
	if query != "" {
		// 搜索固定回第一页，与 JSON 版一致（部分站点 pg 与 wd 并存时会错页）。
		params["wd"] = query
		params["pg"] = "1"
		requestedPage = 1
	}
	document, err := d.libretvXMLAPI(ctx, site, params)
	if err != nil {
		return nil, false, err
	}
	source := libretvSourceID(site.Key)
	var dramas []Drama
	for _, video := range document.List.Videos {
		row := video.row()
		if row.Name == "" {
			continue
		}
		dramas = append(dramas, libretvDramaFromRow(source, row, ""))
	}
	hasMore := false
	if pageCount, _ := strconv.Atoi(strings.TrimSpace(document.List.PageCount)); pageCount > 0 {
		hasMore = requestedPage < pageCount
	} else {
		limit, _ := strconv.Atoi(strings.TrimSpace(document.List.PageSize))
		if limit <= 0 {
			limit = 20
		}
		hasMore = len(dramas) >= limit
	}
	return dramas, hasMore, nil
}

// fetchLibreTVCategoriesXML 拉取分类（ac=list 的 <class> 节点）。
func (d *Downloader) fetchLibreTVCategoriesXML(ctx context.Context, site libretvSite) ([]nativeCategory, error) {
	document, err := d.libretvXMLAPI(ctx, site, map[string]string{"ac": "list"})
	if err != nil {
		return nil, err
	}
	var categories []nativeCategory
	for _, entry := range document.Class.Types {
		id := strings.TrimSpace(entry.ID)
		name := strings.TrimSpace(entry.Name)
		if id == "" || name == "" || len([]rune(name)) > 24 {
			continue
		}
		categories = append(categories, nativeCategory{ID: id, Name: name})
	}
	return categories, nil
}

// fetchLibreTVDetailXML 拉取 XML 详情：剧集元数据 + 各播放源分集。
func (d *Downloader) fetchLibreTVDetailXML(ctx context.Context, site libretvSite, vodID string) (Drama, []Chapter, error) {
	document, err := d.libretvXMLAPI(ctx, site, map[string]string{"ac": "detail", "ids": vodID})
	if err != nil {
		return Drama{}, nil, err
	}
	if len(document.List.Videos) == 0 {
		return Drama{}, nil, errors.New("片源接口没有返回该剧的详情")
	}
	video := document.List.Videos[0]
	source := libretvSourceID(site.Key)
	row := video.row()
	if row.ID == "" {
		row.ID = vodID
	}
	drama := libretvDramaFromRow(source, row, video.description())
	referer := site.API
	if parsed, err := url.Parse(site.API); err == nil {
		referer = parsed.Scheme + "://" + parsed.Host + "/"
	}
	playFrom, playGroups := video.playGroups()
	return drama, libretvChaptersFromGroups(source, vodID, referer, playFrom, playGroups), nil
}
