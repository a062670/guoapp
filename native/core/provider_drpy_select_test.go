package core

import "testing"

// drpy 选择器方言（pdfh/pdfa/pd）的 Go 实现单测。
// 用例对齐 drpy 脚本里的常见写法：后代链、备选、属性取值。

func TestDrpyPDFHDescendantChainText(t *testing.T) {
	body := `<html><body><ul class="list">
<li><a href="/a">甲</a></li>
<li><a href="/b">乙</a></li>
</ul><div class="other"><a href="/c">丙</a></div></body></html>`
	if got := drpyPDFH(body, "body&&.list&&li&&a&&Text"); got != "甲" {
		t.Fatalf(".list 内首个 Text 应为「甲」，得到 %q", got)
	}
	// 备选：前者（不存在的 #zzz）无结果时回退到后者。
	if got := drpyPDFH(body, "#zzz&&a&&Text||.other&&a&&Text"); got != "丙" {
		t.Fatalf("备选分支应取「丙」，得到 %q", got)
	}
}

func TestDrpyPDFHIDChainAndAlternative(t *testing.T) {
	body := `<div id="a"><h2>标题甲</h2><h2>标题乙</h2></div><div class="b">备选文本</div>`
	if got := drpyPDFH(body, "#a&&h2&&Text"); got != "标题甲" {
		t.Fatalf("#a&&h2&&Text 应为「标题甲」，得到 %q", got)
	}
	if got := drpyPDFH(body, "#missing&&h2&&Text||.b&&Text"); got != "备选文本" {
		t.Fatalf("备选链应取「备选文本」，得到 %q", got)
	}
}

func TestDrpyPDFHAttributeValues(t *testing.T) {
	body := `<ul class="item">
<li><a href="/play/1">一</a></li>
<li><a href="/play/2">二</a></li>
</ul>
<div class="pic"><img data-original="https://img.test/a.jpg" src="placeholder.gif"></div>
<div class="lazy"><img data-src="https://img.test/b.jpg"></div>`
	if got := drpyPDFH(body, ".item&&a&&href"); got != "/play/1" {
		t.Fatalf(".item&&a&&href 应为「/play/1」，得到 %q", got)
	}
	// 属性名即取值方式：data-original 优先于 src 的写法。
	if got := drpyPDFH(body, ".pic&&img&&data-original"); got != "https://img.test/a.jpg" {
		t.Fatalf("data-original 取值异常: %q", got)
	}
	if got := drpyPDFH(body, ".lazy&&img&&data-src"); got != "https://img.test/b.jpg" {
		t.Fatalf("data-src 取值异常: %q", got)
	}
	// [attr] 仅要求存在。
	if got := drpyPDFH(body, "img[data-src]&&data-src"); got != "https://img.test/b.jpg" {
		t.Fatalf("[attr] 存在性匹配异常: %q", got)
	}
	// [attr=value] 等值匹配。
	if got := drpyPDFH(body, `[class=lazy]&&img&&data-src`); got != "https://img.test/b.jpg" {
		t.Fatalf("[attr=value] 匹配异常: %q", got)
	}
}

func TestDrpyPDFAMultipleValues(t *testing.T) {
	body := `<div class="rows">
<span>ROW-1</span><span>ROW-2</span><span>ROW-3</span>
</div>`
	values := drpyPDFA(body, ".rows&&span&&Text")
	if len(values) != 3 || values[0] != "ROW-1" || values[2] != "ROW-3" {
		t.Fatalf("pdfa 应取到 3 个值，得到 %+v", values)
	}
	// 前者备选有结果时不再走后者。
	if values := drpyPDFA(body, ".rows&&span&&Text||.zzz&&a&&Text"); len(values) != 3 {
		t.Fatalf("备选在有结果时不应生效，得到 %+v", values)
	}
}

func TestDrpyPDThirdArgument(t *testing.T) {
	body := `<ul class="links"><li><a href="/x">链接</a></li></ul>`
	// pd(html, rule, 'href') 等价于 rule 末段补 &&href。
	rule := ".links&&li&&a"
	merged := rule + "&&href"
	if got := drpyPD(body, merged); got != "/x" {
		t.Fatalf("pd 并段后应为「/x」，得到 %q", got)
	}
	// 不带第三参时默认取 Text。
	if got := drpyPD(body, rule); got != "链接" {
		t.Fatalf("pd 缺省应取 Text「链接」，得到 %q", got)
	}
}

func TestDrpySelectorEdgeCases(t *testing.T) {
	if drpyPDFH("", ".list&&Text") != "" {
		t.Fatal("空 HTML 应返回空串")
	}
	if drpyPDFH("<div>无匹配</div>", ".absent&&Text") != "" {
		t.Fatal("无匹配应返回空串")
	}
	// rule 为空串不应 panic。
	if got := drpyPDFH("<div>x</div>", ""); got != "" {
		t.Fatalf("空 rule 应返回空串，得到 %q", got)
	}
	// 仅一个取值段（无选择器段）时按全文第一个元素文本？drpy 语义下至少一段
	// 选择器，这里验证不 panic 且返回空。
	if got := drpyPDFH("<div>x</div>", "Text"); got != "" {
		t.Fatalf("纯取值段应返回空串，得到 %q", got)
	}
}
