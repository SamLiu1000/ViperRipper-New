// importv 把 2018 年原版（Java 版）ViperRipper 的 user/hosts.xml 规则
// 转换成本项目（Go/Wails 版）的 JSON 规则，写进规则目录里。
//
//	go run ./cmd/importv -xml "H:\aaaaaa软件仓库\ViperRipper Portable\user\hosts.xml" -out build/bin/data/hosts
//
// 默认跳过已有规则覆盖的域名；-enable=false 导入为禁用；-dry-run 只统计不落盘。
package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"viperipper/internal/host"
)

// noteMarker 标在 note 里，用于区分“本工具导入的”和“手写/内置的”，
// 这样重复执行只会刷新自己导的那批，不会盖掉用户改过的规则。
const noteMarker = host.ImportedNoteMarker

// isLegacyImport 判断一条规则是不是本工具导出的（含历史版本的措辞），
// 是的话允许重复导入刷新，手写/内置规则不覆盖。
func isLegacyImport(note string) bool {
	return host.IsImportedNote(note) || strings.Contains(note, noteMarker)
}

// ---------- 原版 hosts.xml 的结构（Java XStream 导出格式） ----------

type xmlList struct {
	XMLName xml.Name     `xml:"list"`
	Hosts   []xmlImgHost `xml:"imgHost"`
}

type xmlImgHost struct {
	Name                string     `xml:"name"`
	ImagePattern        string     `xml:"imagePattern"`
	ImageAction         string     `xml:"imageAction"`
	PostURL             string     `xml:"postUrl"`
	FileName            string     `xml:"fileName"`
	RedirectURLPattern  string     `xml:"redirectUrlPattern"`
	UseThumb            string     `xml:"useThumb"`
	WhatFrom            string     `xml:"whatFrom"`
	WhatTo              string     `xml:"whatTo"`
	HasOriginalFileName string     `xml:"hasOriginalFileName"`
	PostParams          *xmlParams `xml:"postParams"`
}

type xmlParams struct {
	Entries []struct {
		Strings []string `xml:"string"`
	} `xml:"entry"`
}

// ---------- CSS 选择器（原版用 jsoup）→ XPath（本项目引擎只认 XPath） ----------

var tagRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*`)

// cssToXPath 支持的形式覆盖了 hosts.xml 里全部 180+ 条规则：
//
//	img.pic                    → 按 class 取
//	img#main-image             → @id
//	img[id=image]              → 精确属性值
//	img[class~=(centred|centred_resized)]  → 多个 contains 相或
//	a[href*=viewer] / img[src^=x] / img[x$=y] / a[download] / img[class="a b"]
func cssToXPath(sel string) (string, error) {
	s := strings.TrimSpace(sel)
	if s == "" {
		return "", fmt.Errorf("空选择器")
	}
	m := tagRe.FindString(s)
	if m == "" {
		return "", fmt.Errorf("选择器没有标签名：%q", sel)
	}
	tag := strings.ToLower(m)
	rest := s[len(m):]

	var preds []string
	for len(rest) > 0 {
		switch rest[0] {
		case '#':
			val, n := scanWord(rest[1:])
			p, err := eqPred("id", val)
			if err != nil {
				return "", err
			}
			preds = append(preds, p)
			rest = rest[1+n:]
		case '.':
			val, n := scanWord(rest[1:])
			p, err := classTokenPred(val)
			if err != nil {
				return "", err
			}
			preds = append(preds, p)
			rest = rest[1+n:]
		case '[':
			end := strings.Index(rest, "]")
			if end < 0 {
				return "", fmt.Errorf("方括号没有闭合：%q", sel)
			}
			p, err := attrPred(strings.TrimSpace(rest[1:end]))
			if err != nil {
				return "", err
			}
			preds = append(preds, p)
			rest = rest[end+1:]
		default:
			return "", fmt.Errorf("不支持的选择器片段 %q（原选择器 %q）", rest, sel)
		}
	}
	xp := "//" + tag
	if len(preds) > 0 {
		xp += "[" + strings.Join(preds, " and ") + "]"
	}
	return xp, nil
}

// scanWord 取到下一个分隔符（. # [）为止，返回内容与其长度。
func scanWord(s string) (string, int) {
	end := strings.IndexAny(s, ".#[")
	if end < 0 {
		return strings.TrimSpace(s), len(s)
	}
	return strings.TrimSpace(s[:end]), end
}

func attrPred(inner string) (string, error) {
	for _, op := range []string{"~=", "*=", "^=", "$="} {
		if i := strings.Index(inner, op); i > 0 {
			return opPred(op, strings.TrimSpace(inner[:i]), strings.TrimSpace(inner[i+len(op):]))
		}
	}
	if i := strings.Index(inner, "="); i > 0 {
		return eqPred(strings.TrimSpace(inner[:i]), strings.TrimSpace(inner[i+1:]))
	}
	// 裸属性：a[download] → @download
	return "@" + attrName(strings.TrimSuffix(inner, "/")), nil
}

func opPred(op, name, val string) (string, error) {
	name = attrName(name)
	if op == "~=" {
		// jsoup 的 [attr~=regex]：hosts.xml 里只用来写 class 的备选值
		var parts []string
		for _, alt := range splitAlts(val) {
			lit, err := xpathLit(alt)
			if err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("contains(@%s,%s)", name, lit))
		}
		switch len(parts) {
		case 0:
			return "", fmt.Errorf("空的备选取值：%q", val)
		case 1:
			return parts[0], nil
		default:
			return "(" + strings.Join(parts, " or ") + ")", nil
		}
	}
	lit, err := xpathLit(val)
	if err != nil {
		return "", err
	}
	switch op {
	case "*=":
		return fmt.Sprintf("contains(@%s,%s)", name, lit), nil
	case "^=":
		return fmt.Sprintf("starts-with(@%s,%s)", name, lit), nil
	default:
		// XPath 1.0 没有 ends-with，用 substring 兜住
		return fmt.Sprintf("substring(@%s, string-length(@%s)-string-length(%s)+1)=%s", name, name, lit, lit), nil
	}
}

func eqPred(name, val string) (string, error) {
	name = attrName(name)
	val = unquote(val)
	if name == "class" {
		// 多值 class（img[class=mainImage img-thumbnail]）整体包含更安全
		if strings.Contains(val, " ") {
			lit, err := xpathLit(val)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("contains(@class,%s)", lit), nil
		}
		return classTokenPred(val)
	}
	lit, err := xpathLit(val)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("@%s=%s", name, lit), nil
}

func classTokenPred(val string) (string, error) {
	lit, err := xpathLit(" " + val + " ")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("contains(concat(' ',normalize-space(@class),' '),%s)", lit), nil
}

func attrName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// class 之外的属性名照抄；去掉可能混进来的非法字符
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return -1
	}, s)
}

func splitAlts(val string) []string {
	val = strings.TrimSpace(val)
	val = strings.TrimPrefix(val, "(")
	val = strings.TrimSuffix(val, ")")
	var out []string
	for _, p := range strings.Split(val, "|") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// xpathLit 生成 XPath 字符串字面量；XPath 1.0 不允许转义引号，
// 所以只能靠换引号种类，两种引号都出现时这条规则没法翻译。
func xpathLit(v string) (string, error) {
	switch {
	case strings.Contains(v, "'") && strings.Contains(v, `"`):
		return "", fmt.Errorf("取值 %q 同时包含单双引号，XPath 1.0 表达不出来", v)
	case strings.Contains(v, "'"):
		// 值里有单引号就用双引号包
		return `"` + v + `"`, nil
	default:
		return "'" + v + "'", nil
	}
}

// ---------- 规则生成 ----------

// buildRule 把一条原版 <imgHost> 转成我们的 Rule。拿不准的直接返回错误，
// 由调用方统计跳过原因 —— 宁可少导入，也不污染规则目录。
func buildRule(h xmlImgHost, enable bool) (*host.Rule, error) {
	domain := strings.ToLower(strings.TrimSpace(h.Name))
	domain = strings.TrimPrefix(domain, "www.")
	if domain == "" || !strings.Contains(domain, ".") {
		return nil, fmt.Errorf("域名不合法：%q", h.Name)
	}
	r := &host.Rule{
		Name:    domain,
		Domains: []string{domain},
		Referer: "https://" + domain + "/",
		Enabled: enable,
	}
	pattern := strings.TrimSpace(h.ImagePattern)

	var steps []host.Step
	switch {
	case strings.TrimSpace(h.RedirectURLPattern) != "":
		// NEED_GET：先顺着页面上一个链接跳到真正的看图页，再解析。
		sel, err := cssToXPath(h.RedirectURLPattern)
		if err != nil {
			return nil, fmt.Errorf("跳转选择器 %q：%w", h.RedirectURLPattern, err)
		}
		steps = append(steps,
			host.Step{Action: "get", Source: "{page_url}"},
			// 写回 page_url 而不是新变量：白名单里的变量才不会被体检报警
			host.Step{Action: "extract", Field: "page_url", Selector: sel, Attr: linkAttr(h.RedirectURLPattern)},
			host.Step{Action: "get", Source: "{page_url}"},
		)
	case strings.EqualFold(strings.TrimSpace(h.ImageAction), "NEED_POST"):
		steps = append(steps, host.Step{Action: "get", Source: "{page_url}"})
		data := map[string]string{}
		if h.PostParams != nil {
			for _, ent := range h.PostParams.Entries {
				if len(ent.Strings) < 2 {
					continue
				}
				key := strings.TrimSpace(ent.Strings[0])
				raw := strings.TrimSpace(ent.Strings[1])
				if key == "" {
					continue
				}
				field := "pp_" + attrName(key)
				// regex: 前缀表示值要从页面源码里正则捞出（原版语义），
				// 正好对应我们的 search 步骤（取第一个捕获组）。
				if reSrc, ok := strings.CutPrefix(raw, "regex:"); ok {
					steps = append(steps, host.Step{Action: "search", Source: "{body}", Field: field, Selector: reSrc})
					data[key] = "{" + field + "}"
					continue
				}
				// direct: 前缀表示值是字面量，不是选择器（原版语义）
				if lit, ok := strings.CutPrefix(raw, "direct:"); ok {
					data[key] = lit
					continue
				}
				sel, err := cssToXPath(raw)
				if err != nil {
					return nil, fmt.Errorf("表单参数 %s 的选择器 %q：%w", key, raw, err)
				}
				steps = append(steps, host.Step{Action: "extract", Field: field, Selector: sel, Attr: "value"})
				data[key] = "{" + field + "}"
			}
		}
		postURL := strings.TrimSpace(h.PostURL)
		if postURL == "" || postURL == "#imageUrl#" {
			postURL = "{page_url}"
		} else {
			postURL = strings.ReplaceAll(postURL, "#imageUrl#", "{page_url}")
		}
		steps = append(steps, host.Step{Action: "post", Source: postURL, Data: data})
	case pattern == "":
		// 既没给 imagePattern 又没有可用的缩略图改写规则
		if strings.TrimSpace(h.UseThumb) == "" || strings.TrimSpace(h.WhatFrom) == "" {
			return nil, fmt.Errorf("既没有 imagePattern 也没有可用的缩略图改写规则")
		}
		steps = append(steps, thumbSteps(h.WhatFrom, h.WhatTo)...)
		steps = append(steps, host.Step{Action: "set", Field: "filename", Value: "{url_name}"})
	default:
		// NEED_NOTHING：图片直接就在图片页里
		steps = append(steps, host.Step{Action: "get", Source: "{page_url}"})
	}

	if pattern != "" {
		sel, err := cssToXPath(pattern)
		if err != nil {
			return nil, fmt.Errorf("图片选择器 %q：%w", pattern, err)
		}
		steps = append(steps, host.Step{Action: "extract", Field: "image_url", Selector: sel, Attr: imageAttr(pattern)})
		st, ok := filenameStep(h.FileName, pattern)
		if !ok {
			// 原版这条没给文件名线索（<fileName> 是空的）：先在图片节点上试 alt，
			// 取不到就退回 URL 末段，总好过全落成 001.jpg
			st, ok = filenameStep("alt|"+pattern, pattern)
		}
		if ok {
			steps = append(steps, st)
		}
	}
	r.Steps = steps
	r.Note = noteFor(h)
	return r, nil
}

// thumbSteps 生成“缩略图路径改写”步骤：{thumb_url} 里把 from 换成 to
// （原版的 useThumb/whatFrom/whatTo，例如 small → big）。
func thumbSteps(from, to string) []host.Step {
	return []host.Step{
		{Action: "regex", Field: "image_url", Source: "{thumb_url}", Selector: "^(.+)$", Replace: "${1}"},
		{Action: "regex", Field: "image_url", Selector: regexp.QuoteMeta(strings.TrimSpace(from)), Replace: strings.TrimSpace(to)},
		// 兜底：{thumb_url} 没值时把字面量清空，让引擎明确报错而不是拿假地址去下
		{Action: "regex", Field: "image_url", Selector: `^\{thumb_url\}$`},
	}
}

// imageAttr 决定从命中节点上取哪个属性：img 取 src，链接类取 href，meta 取 content。
func imageAttr(pattern string) string {
	p := strings.TrimSpace(pattern)
	switch {
	case strings.HasPrefix(p, "meta["):
		return "content"
	case strings.HasPrefix(p, "link["), strings.HasPrefix(p, "a[") || p == "a":
		return "href"
	default:
		return "src"
	}
}

func linkAttr(pattern string) string {
	if strings.HasPrefix(strings.TrimSpace(pattern), "meta[") {
		return "content"
	}
	return "href"
}

// filenameStep 解析原版 fileName（"attr|selector"）里的属性与选择器。
func filenameStep(fileName, fallbackSel string) (host.Step, bool) {
	attr, sel := "", ""
	if i := strings.Index(fileName, "|"); i >= 0 {
		attr, sel = strings.TrimSpace(fileName[:i]), strings.TrimSpace(fileName[i+1:])
	} else {
		attr = strings.TrimSpace(fileName)
	}
	if attr == "" {
		return host.Step{}, false
	}
	attr = strings.ToLower(attr)
	if sel == "" {
		sel = fallbackSel
	}
	xp, err := cssToXPath(sel)
	if err != nil {
		return host.Step{}, false
	}
	return host.Step{Action: "extract", Field: "filename", Selector: xp, Attr: attr, Default: "{url_name}"}, true
}

func noteFor(h xmlImgHost) string {
	action := strings.TrimSpace(h.ImageAction)
	if action == "" {
		action = "NEED_NOTHING"
	}
	parts := []string{noteMarker, "imageAction=" + action}
	if strings.TrimSpace(h.UseThumb) != "" {
		parts = append(parts, fmt.Sprintf("原规则提示可由缩略图推导（%s→%s）", h.WhatFrom, h.WhatTo))
	}
	parts = append(parts, "未经实机验证，下载异常时在规则列表里关掉即可")
	return strings.Join(parts, "；")
}

// ---------- main ----------

func main() {
	src := flag.String("xml", "", "原版 hosts.xml 路径")
	out := flag.String("out", "build/bin/data/hosts", "规则输出目录")
	enable := flag.Bool("enable", true, "导入的规则是否直接启用")
	dry := flag.Bool("dry-run", false, "只统计，不写文件")
	overwrite := flag.Bool("overwrite", false, "覆盖同名规则文件")
	flag.Parse()

	if *src == "" {
		fmt.Fprintln(os.Stderr, "用法：importv -xml <hosts.xml> [-out <规则目录>] [-enable=false] [-dry-run]")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取失败：", err)
		os.Exit(1)
	}
	var list xmlList
	if err := xml.Unmarshal(raw, &list); err != nil {
		fmt.Fprintln(os.Stderr, "解析 XML 失败：", err)
		os.Exit(1)
	}

	dir, _ := filepath.Abs(*out)
	covered, files, err := existingRules(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取现有规则失败：", err)
		os.Exit(1)
	}

	var written, skippedCover, skippedDup, failed int
	var failList []string
	dupSeen := map[string]bool{}
	for _, h := range list.Hosts {
		domain := strings.ToLower(strings.TrimSpace(h.Name))
		if dupSeen[domain] {
			skippedDup++
			continue
		}
		dupSeen[domain] = true
		if domainCovered(covered, domain) {
			skippedCover++
			continue
		}
		rule, cerr := buildRule(h, *enable)
		if cerr != nil {
			failed++
			failList = append(failList, fmt.Sprintf("  %s：%v", domain, cerr))
			continue
		}
		if verr := host.ValidateRule(rule); verr != nil {
			failed++
			failList = append(failList, fmt.Sprintf("  %s：自检未通过 %v", domain, verr))
			continue
		}
		file := filepath.Join(dir, rule.Name+".custom.json")
		if _, exists := files[strings.ToLower(file)]; exists && !*overwrite {
			skippedCover++
			continue
		}
		if *dry {
			written++
			continue
		}
		blob, merr := host.MarshalRule(rule)
		if merr != nil {
			failed++
			failList = append(failList, fmt.Sprintf("  %s：序列化失败 %v", domain, merr))
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "创建目录失败：", err)
			os.Exit(1)
		}
		if err := os.WriteFile(file, blob, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "写入失败：", err)
			os.Exit(1)
		}
		files[strings.ToLower(file)] = true
		written++
	}

	fmt.Printf("源 %s：共 %d 条\n", filepath.Base(*src), len(list.Hosts))
	fmt.Printf("  导入 %d，已有规则覆盖跳过 %d，xml 内重复跳过 %d，无法转换 %d\n",
		written, skippedCover, skippedDup, failed)
	if len(failList) > 0 {
		sort.Strings(failList)
		fmt.Println("  转换失败明细：")
		for _, l := range failList {
			fmt.Println(l)
		}
	}
	if *dry {
		fmt.Println("  （dry-run，未落盘）")
	} else {
		fmt.Println("  输出目录：", dir)
	}
}

// existingRules 读取目标目录里已有规则的域名，用于去重。
func existingRules(dir string) (map[string]bool, map[string]bool, error) {
	covered := map[string]bool{}
	files := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return covered, files, nil
		}
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		rule, err := host.ParseRule(raw)
		if err != nil {
			continue
		}
		// 上次由本工具导入的规则可以重复刷新；手写规则和内置规则不会被覆盖，
		// 它们的域名也不会被“已有覆盖”误判。
		if isLegacyImport(rule.Note) {
			files[strings.ToLower(filepath.Join(dir, e.Name()))] = true
			continue
		}
		for _, d := range rule.Domains {
			covered[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "www."))] = true
		}
		files[strings.ToLower(filepath.Join(dir, e.Name()))] = true
	}
	return covered, files, nil
}

// domainCovered 已有规则的域名（或其父域）是否已经覆盖这条原版规则。
func domainCovered(covered map[string]bool, domain string) bool {
	if covered[domain] {
		return true
	}
	for p := strings.Index(domain, "."); p >= 0; p = strings.Index(domain[p+1:], ".") {
		parent := domain[p+1:]
		if covered[parent] {
			return true
		}
	}
	return false
}
