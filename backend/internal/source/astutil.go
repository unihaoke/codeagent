// Package source 实现第七层（源码侧）：git 镜像管理、版本锁定、懒加载与
// 文件级缓存、多语言堆栈解析、轻量 AST 语义工具以及多仓库关联匹配打分。
//
// 本包不依赖任何第三方代码解析器，全部能力基于词法扫描 + 正则的确定性实现，
// 保证离线、无网络、无副作用，可被技能层与引擎层直接复用。
package source

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 语言标识常量（DetectLanguage 的取值集合）
// ---------------------------------------------------------------------------

const (
	// LangGo Go 语言。
	LangGo = "go"
	// LangJava Java 语言。
	LangJava = "java"
	// LangPython Python 语言。
	LangPython = "python"
	// LangTypeScript TypeScript 语言。
	LangTypeScript = "typescript"
	// LangJavaScript JavaScript 语言。
	LangJavaScript = "javascript"
	// LangPHP PHP 语言。
	LangPHP = "php"
	// LangCSharp C# 语言。
	LangCSharp = "csharp"
	// LangRust Rust 语言。
	LangRust = "rust"
	// LangYAML YAML 配置。
	LangYAML = "yaml"
	// LangJSON JSON 配置。
	LangJSON = "json"
	// LangSQL SQL 脚本。
	LangSQL = "sql"
	// LangMarkdown Markdown 文档。
	LangMarkdown = "markdown"
	// LangText 纯文本（兜底）。
	LangText = "text"
)

// maxSymbols 单文件最多提取的符号数。
const maxSymbols = 200

// ---------------------------------------------------------------------------
// 语言探测
// ---------------------------------------------------------------------------

// 扩展名 → 语言映射（按扩展名精确判定，优先级最高）。
var extLanguage = map[string]string{
	".go":       LangGo,
	".java":     LangJava,
	".kt":       "kotlin",
	".kts":      "kotlin",
	".scala":    "scala",
	".groovy":   "groovy",
	".py":       LangPython,
	".pyi":      LangPython,
	".pyw":      LangPython,
	".ts":       LangTypeScript,
	".tsx":      LangTypeScript,
	".mts":      LangTypeScript,
	".cts":      LangTypeScript,
	".js":       LangJavaScript,
	".jsx":      LangJavaScript,
	".mjs":      LangJavaScript,
	".cjs":      LangJavaScript,
	".vue":      LangTypeScript,
	".php":      LangPHP,
	".phtml":    LangPHP,
	".cs":       LangCSharp,
	".rs":       LangRust,
	".yaml":     LangYAML,
	".yml":      LangYAML,
	".json":     LangJSON,
	".jsonc":    LangJSON,
	".sql":      LangSQL,
	".md":       LangMarkdown,
	".markdown": LangMarkdown,
	".txt":      LangText,
	".xml":      "xml",
	".html":     "html",
	".htm":      "html",
	".css":      "css",
	".scss":     "css",
	".less":     "css",
	".sh":       "shell",
	".bash":     "shell",
	".zsh":      "shell",
	".ps1":      "powershell",
	".rb":       "ruby",
	".c":        "c",
	".h":        "c",
	".cc":       "cpp",
	".cpp":      "cpp",
	".hpp":      "cpp",
	".gradle":   "groovy",
	".proto":    "protobuf",
}

// 无扩展名/特殊文件名 → 语言映射。
var nameLanguage = map[string]string{
	"go.mod":        LangGo,
	"go.sum":        LangGo,
	"makefile":      "makefile",
	"dockerfile":    "dockerfile",
	"pom.xml":       "xml",
	"build.gradle":  "groovy",
	"package.json":  LangJSON,
	"tsconfig.json": LangJSON,
	".gitignore":    LangText,
}

// DetectLanguage 依据文件扩展名与内容特征判定语言。
//
// 扩展名优先；扩展名缺失或不足以区分（如 .js 中的 TypeScript、无扩展名脚本）时
// 回退到内容特征判定（shebang、`package main`、`<?php`、`import React` 等）。
// 返回值取 go/java/python/typescript/javascript/php/csharp/rust/yaml/json/sql/
// markdown/text 之一（另有少量常见语言的扩展名直出）。
func DetectLanguage(path string, content []byte) string {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(path, "\\", "/")))
	ext := strings.ToLower(filepath.Ext(base))

	if v, ok := nameLanguage[base]; ok {
		return v
	}
	if v, ok := extLanguage[ext]; ok {
		// .js 可能是 TypeScript 编译产物或 TS 源码，用内容特征细分。
		if v == LangJavaScript || v == LangTypeScript {
			return detectJSFamily(content, v)
		}
		return v
	}
	return detectByContent(content)
}

// detectJSFamily 区分 JavaScript / TypeScript。
func detectJSFamily(content []byte, fallback string) string {
	s := string(content)
	tsMarkers := []string{
		": string", ": number", ": boolean", ": void", ": any", ": unknown",
		"interface ", "type ", "enum ", "implements ", "declare ", "readonly ",
		"<T>", "as const", "satisfies ", "export type",
	}
	for _, m := range tsMarkers {
		if strings.Contains(s, m) {
			return LangTypeScript
		}
	}
	// 形如 `const x: Foo =` 的注解
	if tsAnnotationRe.MatchString(s) {
		return LangTypeScript
	}
	if fallback != "" {
		return fallback
	}
	return LangJavaScript
}

var tsAnnotationRe = regexp.MustCompile(`(?m)\b(?:const|let|var|function)\s+\w+\s*:\s*[A-Za-z_$]`)

// detectByContent 在没有可用扩展名时按内容特征判定语言。
func detectByContent(content []byte) string {
	if len(content) == 0 {
		return LangText
	}
	head := content
	if len(head) > 8192 {
		head = head[:8192]
	}
	s := string(head)
	trimmed := strings.TrimSpace(s)

	switch {
	case strings.Contains(s, "<?php"):
		return LangPHP
	case strings.HasPrefix(s, "#!/usr/bin/env python"), strings.HasPrefix(s, "#!/usr/bin/python"),
		strings.Contains(s, "if __name__ =="):
		return LangPython
	case strings.HasPrefix(s, "#!/bin/sh"), strings.HasPrefix(s, "#!/bin/bash"),
		strings.HasPrefix(s, "#!/usr/bin/env bash"):
		return "shell"
	case strings.Contains(s, "package main"), strings.Contains(s, "func main("):
		return LangGo
	case strings.Contains(s, "import React"), strings.Contains(s, "from 'react'"),
		strings.Contains(s, `from "react"`), strings.Contains(s, "require('react')"),
		strings.Contains(s, "export default"), strings.Contains(s, "module.exports"),
		strings.Contains(s, "console.log("):
		return detectJSFamily(content, LangJavaScript)
	case strings.HasPrefix(trimmed, "{"):
		return LangJSON
	case strings.HasPrefix(trimmed, "package ") && strings.HasSuffix(trimmed, ";"),
		strings.Contains(s, "public class "), strings.Contains(s, "public static void main"):
		return LangJava
	case strings.Contains(s, "using System;"), strings.Contains(s, "namespace "):
		return LangCSharp
	case strings.Contains(s, "fn main("), strings.Contains(s, "pub struct "):
		return LangRust
	case strings.HasPrefix(trimmed, "<?xml"):
		return "xml"
	case strings.HasPrefix(trimmed, "# "), strings.HasPrefix(trimmed, "## "):
		return LangMarkdown
	}
	return LangText
}

// GuessLanguageFamily 将具体语言归族，供沙箱选择校验命令。
//
// 返回 jvm / golang / python / node / php / other 之一。
func GuessLanguageFamily(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "java", "kotlin", "scala", "groovy", "jvm", "clojure":
		return "jvm"
	case "go", "golang":
		return "golang"
	case "python", "python3":
		return "python"
	case "javascript", "typescript", "js", "ts", "node", "nodejs", "vue":
		return "node"
	case "php":
		return "php"
	default:
		return "other"
	}
}

// ---------------------------------------------------------------------------
// 符号提取
// ---------------------------------------------------------------------------

var (
	goFuncRe   = regexp.MustCompile(`(?m)^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*\(`)
	goTypeRe   = regexp.MustCompile(`(?m)^type\s+([A-Za-z_]\w*)\s`)
	goValueRe  = regexp.MustCompile(`(?m)^(?:const|var)\s+(?:\(\s*)?([A-Za-z_]\w*)`)
	goValue2Re = regexp.MustCompile(`(?m)^\t([A-Za-z_]\w*)\s*(?:[A-Za-z_\[\]*.]+)?\s*=`)

	javaTypeRe   = regexp.MustCompile(`(?m)^[ \t]*(?:(?:public|protected|private|abstract|final|static|sealed|non-sealed|strictfp)\s+)*(class|interface|enum|record|@interface)\s+([A-Za-z_]\w*)`)
	javaMethodRe = regexp.MustCompile(
		`(?m)^[ \t]+(?:(?:public|protected|private|static|final|abstract|synchronized|native|default|strictfp)\s+)*` +
			`(?:<[^>\n]{1,80}>\s*)?(?:[\w.<>\[\],?$]+\s+)?([A-Za-z_]\w*)\s*\([^;{}\n]*\)\s*(?:throws\s+[\w.,\s<>]+)?\{`)

	pythonDefRe   = regexp.MustCompile(`(?m)^[ \t]*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	pythonClassRe = regexp.MustCompile(`(?m)^[ \t]*class\s+([A-Za-z_]\w*)`)
	pythonConstRe = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]{2,})\s*[:=]`)

	jsFuncRe  = regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	jsClassRe = regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	jsConstRe = regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\(|function\b|class\b)`)
	jsIfaceRe = regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?(?:declare\s+)?(?:interface|type|enum)\s+([A-Za-z_$][\w$]*)`)
	// 类方法/对象方法：可带修饰符或返回类型注解，且形参列表不含控制流关键字。
	jsMethodRe = regexp.MustCompile(
		`(?m)^[ \t]+(?:(?:public|private|protected|static|async|get|set|override|readonly|abstract)\s+)*\*?\s*` +
			`([A-Za-z_$][\w$]*)\s*\(([^;{}\n()]*)\)\s*(?::\s*[A-Za-z_$][\w$.<>\[\]|, ]*)?\{`)

	phpFuncRe  = regexp.MustCompile(`(?m)^[ \t]*(?:(?:public|protected|private|static|final|abstract)\s+)*function\s+([A-Za-z_]\w*)`)
	phpClassRe = regexp.MustCompile(`(?m)^[ \t]*(?:(?:final|abstract|readonly)\s+)*(?:class|interface|trait|enum)\s+([A-Za-z_]\w*)`)

	csTypeRe = regexp.MustCompile(`(?m)^[ \t]*(?:(?:public|internal|private|protected|static|sealed|abstract|partial)\s+)*(?:class|interface|struct|record|enum)\s+([A-Za-z_]\w*)`)

	rustFnRe   = regexp.MustCompile(`(?m)^[ \t]*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?fn\s+([A-Za-z_]\w*)`)
	rustTypeRe = regexp.MustCompile(`(?m)^[ \t]*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|union)\s+([A-Za-z_]\w*)`)

	sqlObjectRe = regexp.MustCompile(`(?im)^\s*CREATE\s+(?:OR\s+REPLACE\s+)?(?:TABLE|VIEW|INDEX|FUNCTION|PROCEDURE|TRIGGER)\s+(?:IF\s+NOT\s+EXISTS\s+)?([\w."` + "`" + `]+)`)
)

// ExtractSymbols 提取顶层类型 / 函数 / 方法 / 常量名。
//
// 按语言采用词法扫描实现，结果去重并保持出现顺序，最多返回 200 个。
func ExtractSymbols(lang, content string) []string {
	if content == "" {
		return nil
	}
	// 防御性：超长内容只扫描前 2MB，避免 O(n) 之外的额外开销。
	if len(content) > 2<<20 {
		content = content[:2<<20]
	}
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || len(out) >= maxSymbols {
			return
		}
		if isKeyword(name) || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	collect := func(re *regexp.Regexp, group int) {
		for _, m := range re.FindAllStringSubmatch(content, maxSymbols) {
			if len(m) > group {
				add(m[group])
			}
		}
	}

	switch strings.ToLower(strings.TrimSpace(lang)) {
	case LangGo:
		collect(goFuncRe, 1)
		collect(goTypeRe, 1)
		collect(goValueRe, 1)
		collect(goValue2Re, 1)
	case LangJava, "kotlin", "scala", "groovy":
		collect(javaTypeRe, 2)
		collect(javaMethodRe, 1)
	case LangPython:
		collect(pythonDefRe, 1)
		collect(pythonClassRe, 1)
		collect(pythonConstRe, 1)
	case LangTypeScript, LangJavaScript, "vue":
		collect(jsFuncRe, 1)
		collect(jsClassRe, 1)
		collect(jsIfaceRe, 1)
		collect(jsConstRe, 1)
		collectJSMethods(content, add)
	case LangPHP:
		collect(phpFuncRe, 1)
		collect(phpClassRe, 1)
	case LangCSharp:
		collect(csTypeRe, 1)
	case LangRust:
		collect(rustFnRe, 1)
		collect(rustTypeRe, 1)
	case LangSQL:
		collect(sqlObjectRe, 1)
	default:
		// 未识别语言：用最常见的几种声明形态兜底。
		collect(goFuncRe, 1)
		collect(pythonDefRe, 1)
		collect(jsFuncRe, 1)
	}
	return out
}

// collectJSMethods 提取 TS/JS 类或对象中的方法名（过滤控制流关键字）。
func collectJSMethods(content string, add func(string)) {
	for _, m := range jsMethodRe.FindAllStringSubmatch(content, maxSymbols) {
		if len(m) < 2 {
			continue
		}
		name := strings.TrimSpace(m[1])
		switch name {
		case "if", "for", "while", "switch", "catch", "return", "function", "constructor":
			if name != "constructor" {
				continue
			}
		}
		add(name)
	}
}

// isKeyword 过滤掉被误识别为符号的语言关键字。
func isKeyword(name string) bool {
	switch name {
	case "if", "for", "while", "switch", "return", "else", "do", "try", "catch",
		"finally", "new", "delete", "sizeof", "import", "package", "from", "require",
		"func", "def", "class", "type", "const", "var", "let", "public", "private",
		"protected", "static", "void", "int", "string", "bool", "true", "false", "nil":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// import 提取
// ---------------------------------------------------------------------------

var (
	javaImportRe = regexp.MustCompile(`(?m)^\s*import\s+(?:static\s+)?([\w.*]+)\s*;`)
	pyFromRe     = regexp.MustCompile(`(?m)^\s*from\s+([\w.]+)\s+import\s+`)
	pyImportRe   = regexp.MustCompile(`(?m)^\s*import\s+([\w.]+(?:\s*,\s*[\w.]+)*)`)
	jsImportRe   = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?(?:[^'"\n]*?\s+from\s+)?['"]([^'"]+)['"]`)
	jsRequireRe  = regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`)
	jsDynamicRe  = regexp.MustCompile(`import\(\s*['"]([^'"]+)['"]\s*\)`)
	phpUseRe     = regexp.MustCompile(`(?m)^\s*use\s+([\w\\]+)`)
	csUsingRe    = regexp.MustCompile(`(?m)^\s*using\s+([\w.]+)\s*;`)
	rustUseRe    = regexp.MustCompile(`(?m)^\s*use\s+([\w:]+)`)
)

// ExtractImports 提取 Go / Java / Python / TS-JS / PHP 的 import / require 包名。
//
// 返回值不含分号与引号，去重并保持出现顺序。
func ExtractImports(lang, content string) []string {
	if content == "" {
		return nil
	}
	if len(content) > 2<<20 {
		content = content[:2<<20]
	}
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		name = strings.Trim(name, `"'`+";")
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	collect := func(re *regexp.Regexp, group int) {
		for _, m := range re.FindAllStringSubmatch(content, 400) {
			if len(m) > group {
				add(m[group])
			}
		}
	}

	switch strings.ToLower(strings.TrimSpace(lang)) {
	case LangGo:
		for _, v := range goImports(content) {
			add(v)
		}
	case LangJava, "kotlin", "scala", "groovy":
		collect(javaImportRe, 1)
	case LangPython:
		collect(pyFromRe, 1)
		for _, m := range pyImportRe.FindAllStringSubmatch(content, 400) {
			if len(m) < 2 {
				continue
			}
			for _, p := range strings.Split(m[1], ",") {
				add(p)
			}
		}
	case LangTypeScript, LangJavaScript, "vue":
		collect(jsImportRe, 1)
		collect(jsRequireRe, 1)
		collect(jsDynamicRe, 1)
	case LangPHP:
		collect(phpUseRe, 1)
	case LangCSharp:
		collect(csUsingRe, 1)
	case LangRust:
		collect(rustUseRe, 1)
	default:
		collect(javaImportRe, 1)
		collect(jsImportRe, 1)
		collect(pyFromRe, 1)
	}
	return out
}

// goImports 解析 Go 的 import 单行与括号块（不使用 regexp，避免跨行误匹配）。
func goImports(content string) []string {
	var out []string
	inBlock := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if inBlock {
			if line == ")" || strings.HasPrefix(line, ")") {
				inBlock = false
				continue
			}
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			if i := strings.Index(line, "//"); i >= 0 {
				line = strings.TrimSpace(line[:i])
			}
			out = append(out, firstQuoted(line))
			continue
		}
		if !strings.HasPrefix(line, "import") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "import"))
		if strings.HasPrefix(rest, "(") {
			inBlock = true
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "("))
			if strings.HasPrefix(rest, ")") {
				inBlock = false
				continue
			}
			if rest != "" {
				out = append(out, firstQuoted(rest))
			}
			continue
		}
		if rest != "" {
			out = append(out, firstQuoted(rest))
		}
	}
	res := make([]string, 0, len(out))
	for _, v := range out {
		if v != "" {
			res = append(res, v)
		}
	}
	return res
}

// firstQuoted 取一行中第一个双引号字符串；无引号时取第一个字段（兼容别名/裸路径）。
func firstQuoted(line string) string {
	if i := strings.Index(line, `"`); i >= 0 {
		if j := strings.Index(line[i+1:], `"`); j >= 0 {
			return strings.TrimSpace(line[i+1 : i+1+j])
		}
	}
	fields := strings.Fields(strings.ReplaceAll(line, "`", ""))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// ---------------------------------------------------------------------------
// 行工具
// ---------------------------------------------------------------------------

// splitLines 把内容切成行（兼容 \n 与 \r\n；单行以 \r 结尾时剔除 \r）。
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.Split(content, "\n")
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}

// LineCount 返回内容行数（不把结尾换行算作额外一行，空内容为 0）。
func LineCount(content string) int {
	return len(splitLines(content))
}

// SliceLines 取 [start,end] 闭区间行（1-based），越界自动收敛。
//
// 返回切片内容与实际起止行号；区间完全越界时返回空串与 0,0。
func SliceLines(content string, start, end int) (string, int, int) {
	lines := splitLines(content)
	n := len(lines)
	if n == 0 {
		return "", 0, 0
	}
	if start < 1 {
		start = 1
	}
	if end < 1 || end > n {
		end = n
	}
	if start > n {
		return "", 0, 0
	}
	if end < start {
		end = start
	}
	return strings.Join(lines[start-1:end], "\n"), start, end
}

// FocusWindow 以 line 为中心、半径 radius 取一段窗口，返回内容与实际起止行。
func FocusWindow(content string, line, radius int) (string, int, int) {
	if radius < 0 {
		radius = 0
	}
	if line < 1 {
		line = 1
	}
	return SliceLines(content, line-radius, line+radius)
}

// normalizeSnippet 归一化片段：统一换行、去除首尾空白行、剔除每行首尾空白。
func normalizeSnippet(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	// 去首尾空白行
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// FindSnippet 在内容中查找片段，忽略首尾空白与缩进差异。
//
// fromLine 为 1-based 起始行（<=0 表示从首行开始）。命中返回起始行号与 true。
func FindSnippet(content, snippet string, fromLine int) (int, bool) {
	norm := normalizeSnippet(snippet)
	if norm == "" {
		return 0, false
	}
	snorm := normalizeSnippet(content)
	if snorm == "" {
		return 0, false
	}
	if fromLine < 1 {
		fromLine = 1
	}
	needle := strings.Split(norm, "\n")
	hay := strings.Split(snorm, "\n")
	if len(needle) > len(hay) {
		return 0, false
	}
	// 优先从 fromLine 之后查找；未命中则回退到全文查找（同一片段可能重复出现）。
	if line, ok := findNormLines(hay, needle, fromLine-1); ok {
		return line + 1, true
	}
	if fromLine > 1 {
		if line, ok := findNormLines(hay, needle, 0); ok {
			return line + 1, true
		}
	}
	return 0, false
}

// findNormLines 在已归一化的行集合中从 from（0-based）开始做子序列匹配。
func findNormLines(hay, needle []string, from int) (int, bool) {
	if from < 0 {
		from = 0
	}
	last := len(hay) - len(needle)
	for i := from; i <= last; i++ {
		ok := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return i, true
		}
	}
	return 0, false
}

// leadingIndent 返回一行前导空白（空格/制表符）。
func leadingIndent(line string) string {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[:i]
}

// ApplySnippetReplace 把 oldSnippet 替换为 newSnippet，并保持原有缩进。
//
// 缩进策略：按 oldSnippet 首行在 newSnippet 中应有的缩进为基准整体平移 newSnippet
// （即 newSnippet 各行相对首行的缩进层级被完整保留），因此无论模型给出的片段
// 是否带缩进，替换后的代码都能与文件现有缩进风格对齐。
// 返回新内容与被替换的行区间 [startLine,endLine]；片段未命中时返回错误。
func ApplySnippetReplace(content, oldSnippet, newSnippet string, fromLine int) (string, int, int, error) {
	startLine, ok := FindSnippet(content, oldSnippet, fromLine)
	if !ok {
		return content, 0, 0, ErrSnippetNotFound
	}
	oldLines := strings.Split(normalizeSnippet(oldSnippet), "\n")
	n := len(oldLines)
	if n == 0 {
		return content, 0, 0, ErrSnippetNotFound
	}

	raw := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	idx := startLine - 1
	if idx < 0 || idx+n > len(raw) {
		return content, 0, 0, ErrSnippetNotFound
	}

	// 目标缩进：文件中被替换首行的实际缩进（即 oldSnippet 首行在文件里的缩进）。
	targetIndent := leadingIndent(raw[idx])
	// 片段自身首行缩进：作为缩进基准，两者之差即整体平移量。
	oldBase := leadingIndent(oldLines[0])

	newLines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(newSnippet, "\r\n", "\n"), "\r", "\n"), "\n")
	// 去掉 newSnippet 首尾的空行，避免引入多余空行。
	for len(newLines) > 0 && strings.TrimSpace(newLines[0]) == "" {
		newLines = newLines[1:]
	}
	for len(newLines) > 0 && strings.TrimSpace(newLines[len(newLines)-1]) == "" {
		newLines = newLines[:len(newLines)-1]
	}
	indentUnit := detectIndentUnit(targetIndent)
	insideBlock := false
	for i, l := range newLines {
		if strings.TrimSpace(l) == "" {
			newLines[i] = ""
			continue
		}
		rel := relativeIndent(l, oldBase)
		// 片段自身没有缩进信息（模型常给扁平片段）时，用花括号层级补一层缩进。
		if rel == "" && insideBlock && !isBlockCloser(l) {
			rel = indentUnit
		}
		newLines[i] = targetIndent + expandIndent(rel, targetIndent) + strings.TrimLeft(l, " \t")
		insideBlock = opensBlock(l)
	}

	out := make([]string, 0, len(raw)-n+len(newLines))
	out = append(out, raw[:idx]...)
	out = append(out, newLines...)
	out = append(out, raw[idx+n:]...)
	endLine := startLine + len(newLines) - 1
	if endLine < startLine {
		endLine = startLine
	}
	return strings.Join(out, "\n"), startLine, endLine, nil
}

// rebaseIndent 把一行的缩进从 fromBase 基准平移到 toBase 基准，保留相对缩进层级。
//
// 例：oldBase="    " / toBase="    " 时，行 "        return x"（相对缩进 4）→ "            return x"。
func rebaseIndent(line, fromBase, toBase string) string {
	return toBase + expandIndent(relativeIndent(line, fromBase), toBase) + strings.TrimLeft(line, " \t")
}

// detectIndentUnit 从基准缩进推断缩进单元（默认 4 个空格；基准为制表符时用制表符）。
func detectIndentUnit(base string) string {
	if strings.Contains(base, "\t") {
		return "\t"
	}
	if base == "" {
		return "    "
	}
	return base
}

// opensBlock 判断一行是否开启一个新代码块（用于片段扁平时的缩进补齐）。
func opensBlock(line string) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") {
		return false
	}
	return strings.HasSuffix(t, "{") || strings.HasSuffix(t, ":")
}

// isBlockCloser 判断一行是否属于块的收尾（不应额外缩进）。
func isBlockCloser(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return true
	}
	switch t[0] {
	case '}', ')', ']':
		return true
	}
	for _, kw := range []string{"else", "elif", "catch", "finally", "case ", "default:"} {
		if strings.HasPrefix(t, kw) {
			return true
		}
	}
	return false
}

// relativeIndent 返回一行相对基准缩进的额外缩进（已被 TrimLeft 的正文另行拼接）。
func relativeIndent(line, base string) string {
	cur := leadingIndent(line)
	switch {
	case base == "":
		return cur
	case strings.HasPrefix(cur, base):
		return cur[len(base):]
	case strings.HasPrefix(base, cur):
		// 片段缩进比基准浅：视为无额外缩进。
		return ""
	default:
		// 缩进风格不一致（如空格 vs 制表符）：按长度差近似。
		if len(cur) > len(base) {
			return cur[len(base):]
		}
		return ""
	}
}

// expandIndent 把任意空白前缀转换为统一的缩进字符。
func expandIndent(indent, unit string) string {
	if indent == "" {
		return ""
	}
	count := 0
	for _, r := range indent {
		if r == '\t' {
			count += 4
			continue
		}
		count++
	}
	if unit == "\t" {
		return strings.Repeat("\t", count/4)
	}
	return strings.Repeat(" ", count)
}

// ---------------------------------------------------------------------------
// 二进制与文本辅助
// ---------------------------------------------------------------------------

// IsBinary 判断内容是否为二进制（含 NUL 字节，或不可打印字符占比超过 10%）。
func IsBinary(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	sample := content
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if !utf8.Valid(sample) {
		return true
	}
	bad := 0
	for _, b := range sample {
		if b == 0 {
			return true
		}
		if b < 0x09 || (b > 0x0d && b < 0x20) {
			bad++
		}
	}
	return float64(bad)/float64(len(sample)) > 0.10
}

// TruncateUTF8 在不超过 maxBytes 的前提下安全截断字符串，不切断多字节字符。
func TruncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// TruncateChars 截断字符串到 maxChars 个字符（按 rune 计数）。
func TruncateChars(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	i := 0
	for pos := range s {
		if i == maxChars {
			return s[:pos]
		}
		i++
	}
	return s
}

// SortStringsStable 对字符串切片做去重（保序）。
func SortStringsStable(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// sortStringsCopy 返回排序后的副本（供内部确定性输出使用）。
func sortStringsCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
