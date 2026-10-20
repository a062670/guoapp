package core

// drpy HTML 选择器（pdfh/pdfa/pd 的 Go 实现）。
//
// drpy 脚本里的 DOM 解析用「海阔」风格选择器：
//
//	pdfh(html, 'body&&.list&&li&&a&&Text')
//	pdfa(html, '.item&&href||.item&&data-original')
//
// 语法子集：
//   - `||` 备选：前者取不到结果时用后者；
//   - `&&` 分段：首段与中间段是选择器（Tag / #id / .class / [attr=v] 可组合，
//     逐段在前一段结果的后代里缩小范围），末段是取值方式（Text / href /
//     src / data-original 等属性名；缺省取 Text）；
//   - `[attr]`（无等号）只要求属性存在。
//
// 基于 golang.org/x/net/html 自研，不引 goquery：drpy 方言不是纯 CSS，
// goquery 不免这层方言；少一棵依赖树，接口不变可后续替换。

import (
	"strings"

	"golang.org/x/net/html"
)

// drpySelector 单段选择器（或末段的取值方式）。
type drpySelector struct {
	tag      string            // 标签名（空 = 任意）
	id       string            // #id
	classes  []string          // .class（可多个，全部匹配）
	attrs    map[string]string // [attr=value]
	attrOnly string            // [attr] 仅要求存在
	want     string            // 取值："" / "Text" / 属性名
}

// parseDrpySelector 解析单段。
func parseDrpySelector(segment string) drpySelector {
	segment = strings.TrimSpace(segment)
	if segment == "" {
		return drpySelector{}
	}
	sel := drpySelector{}
	// 末段取值方式：Text 或纯属性名（不含选择器字符）。
	if isDrpyWantToken(segment) {
		sel.want = segment
		return sel
	}
	for len(segment) > 0 {
		switch segment[0] {
		case '#':
			rest := segment[1:]
			sel.id, segment = drpyCutToken(rest)
		case '.':
			rest := segment[1:]
			class := ""
			class, segment = drpyCutToken(rest)
			if class != "" {
				sel.classes = append(sel.classes, class)
			}
		case '[':
			end := strings.Index(segment, "]")
			if end < 0 {
				return sel
			}
			inner := strings.TrimSpace(segment[1:end])
			segment = strings.TrimSpace(segment[end+1:])
			if key, value, found := strings.Cut(inner, "="); found {
				if sel.attrs == nil {
					sel.attrs = map[string]string{}
				}
				sel.attrs[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
			} else {
				sel.attrOnly = inner
			}
		default:
			sel.tag, segment = drpyCutToken(segment)
		}
	}
	return sel
}

// isDrpyWantToken 判断是否为取值方式段（Text 或常见属性名）。
func isDrpyWantToken(segment string) bool {
	switch segment {
	case "Text", "text", "TEXT",
		"href", "src", "data-original", "data-src", "data-url", "data-vip",
		"title", "alt", "value", "content":
		return true
	}
	return false
}

// drpyCutToken 截取一段选择器记号（标签/类/ID 共享）。
func drpyCutToken(text string) (token, rest string) {
	for index := 0; index < len(text); index++ {
		switch text[index] {
		case '#', '.', '[':
			return text[:index], text[index:]
		}
	}
	return text, ""
}

// drpySelectorMatches 节点是否匹配选择器（不含 want）。
func drpySelectorMatches(node *html.Node, sel drpySelector) bool {
	if node.Type != html.ElementNode {
		return false
	}
	if sel.tag != "" && node.Data != sel.tag {
		return false
	}
	if sel.id != "" && !drpyHasAttr(node, "id", sel.id) {
		return false
	}
	if sel.attrOnly != "" && !drpyHasAttr(node, sel.attrOnly, "") {
		return false
	}
	for key, value := range sel.attrs {
		if !drpyHasAttr(node, key, value) {
			return false
		}
	}
	for _, class := range sel.classes {
		if !drpyHasClass(node, class) {
			return false
		}
	}
	return true
}

func drpyHasAttr(node *html.Node, key, value string) bool {
	for _, attribute := range node.Attr {
		if attribute.Key != key {
			continue
		}
		if value == "" || attribute.Val == value {
			return true
		}
	}
	return false
}

func drpyHasClass(node *html.Node, class string) bool {
	for _, attribute := range node.Attr {
		if attribute.Key == "class" {
			for _, item := range strings.Fields(attribute.Val) {
				if item == class {
					return true
				}
			}
		}
	}
	return false
}

// drpySelectAll 按分段列表选节点：每段在前一段结果的后代里继续匹配。
func drpySelectAll(root *html.Node, segments []drpySelector) []*html.Node {
	if root == nil || len(segments) == 0 {
		return nil
	}
	current := []*html.Node{root}
	for index, sel := range segments {
		last := index == len(segments)-1
		var next []*html.Node
		seen := map[*html.Node]bool{}
		for _, scope := range current {
			drpyWalk(scope, func(node *html.Node) {
				if seen[node] {
					return
				}
				if drpySelectorMatches(node, sel) {
					seen[node] = true
					next = append(next, node)
				}
			})
		}
		if len(next) == 0 {
			return nil
		}
		current = next
		if last {
			return current
		}
	}
	return current
}

func drpyWalk(node *html.Node, visit func(*html.Node)) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		visit(child)
		drpyWalk(child, visit)
	}
}

// drpyNodeValue 按取值方式提取节点的值。
func drpyNodeValue(node *html.Node, want string) string {
	if node == nil {
		return ""
	}
	if want == "" || strings.EqualFold(want, "Text") || strings.EqualFold(want, "text") {
		return strings.TrimSpace(drpyNodeText(node))
	}
	for _, attribute := range node.Attr {
		if attribute.Key == want {
			return strings.TrimSpace(attribute.Val)
		}
	}
	return ""
}

func drpyNodeText(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

// drpySelectValue 对一条完整 rule（含备选）取第一个匹配值。
func drpySelectValue(root *html.Node, rule string) (string, bool) {
	for _, alternative := range strings.Split(rule, "||") {
		segments := drpySegments(alternative)
		if len(segments) == 0 {
			continue
		}
		want := segments[len(segments)-1].want
		nodes := drpySelectAll(root, segments[:len(segments)-1])
		if len(nodes) == 0 {
			continue
		}
		if want == "" {
			want = "Text"
		}
		value := drpyNodeValue(nodes[0], want)
		if value != "" {
			return value, true
		}
	}
	return "", false
}

// drpySegments 把一段 rule 切成选择器段序列。
func drpySegments(alternative string) []drpySelector {
	var segments []drpySelector
	for _, segment := range strings.Split(alternative, "&&") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		segments = append(segments, parseDrpySelector(segment))
	}
	return segments
}

// drpyPDFH pdfh(html, rule)：取第一个匹配值。
func drpyPDFH(htmlText, rule string) string {
	value, _ := drpySelectValue(drpyParseHTML(htmlText), rule)
	return value
}

// drpyPDFA pdfa(html, rule)：取全部匹配值（每个备选分支独立收集）。
func drpyPDFA(htmlText, rule string) []string {
	root := drpyParseHTML(htmlText)
	var out []string
	for _, alternative := range strings.Split(rule, "||") {
		segments := drpySegments(alternative)
		if len(segments) == 0 {
			continue
		}
		want := segments[len(segments)-1].want
		if want == "" {
			want = "Text"
		}
		for _, node := range drpySelectAll(root, segments[:len(segments)-1]) {
			if value := drpyNodeValue(node, want); value != "" {
				out = append(out, value)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}

// drpyPD pd(html, rule)：drpy 的 pd 第三参是取值类型（Text/href/...），
// 与把类型写进 rule 末段等价；这里直接透传。
func drpyPD(htmlText, rule string) string {
	return drpyPDFH(htmlText, rule)
}

// drpyParseHTML 解析 HTML 文本；解析失败返回 nil（调用方按无结果处理）。
func drpyParseHTML(htmlText string) *html.Node {
	if strings.TrimSpace(htmlText) == "" {
		return nil
	}
	root, err := html.Parse(strings.NewReader(htmlText))
	if err != nil {
		return nil
	}
	return root
}
