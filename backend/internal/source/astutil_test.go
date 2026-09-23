package source

import (
	"strings"
	"testing"
)

const javaSample = `package com.acme.order;

import java.util.List;
import static org.junit.Assert.assertNotNull;

public class OrderService {
    private final OrderRepository repository;

    public OrderService(OrderRepository repository) {
        this.repository = repository;
    }

    public Order create(String userId, int qty) throws OrderException {
        if (userId == null) {
            throw new IllegalArgumentException("userId 不能为空");
        }
        return repository.save(new Order(userId, qty));
    }

    @Override
    public void cancel(String orderId) throws OrderException {
    }
}
`

const pythonSample = `import os
import sys, json
from typing import List

MAX_RETRY = 3


class OrderService:
    def __init__(self, repo):
        self.repo = repo

    async def create(self, user_id: int) -> dict:
        return self.repo.save(user_id)

    def _retry(self):
        pass


def main():
    svc = OrderService(None)
    svc.create(1)
`

const goSample = `package main

import (
	"context"
	"fmt"

	"github.com/codeagent/backend/internal/domain"
)

const maxRetry = 3

var ErrOrder = fmt.Errorf("order error")

type OrderService struct {
	repo string
}

func NewOrderService(repo string) *OrderService {
	return &OrderService{repo: repo}
}

func (s *OrderService) Create(ctx context.Context, id string) error {
	return nil
}
`

const tsSample = `import { Injectable } from '@nestjs/common';
import axios from "axios";
const fs = require('fs');

export interface OrderDto {
  id: string;
}

export class OrderService {
  async create(dto: OrderDto): Promise<string> {
    return dto.id;
  }
}

export function helper(): void {}

const handler = async (req) => req;
`

const phpSample = `<?php

namespace App\Service;

use App\Repository\OrderRepository;

class OrderService
{
    private OrderRepository $repository;

    public function create(int $id): array
    {
        return [];
    }
}
`

func TestDetectLanguage(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		content string
		want    string
	}{
		{"扩展名 go", "internal/source/a.go", goSample, LangGo},
		{"扩展名 java", "src/main/java/com/acme/order/OrderService.java", javaSample, LangJava},
		{"扩展名 python", "app/order/service.py", pythonSample, LangPython},
		{"扩展名 typescript", "src/order.service.ts", tsSample, LangTypeScript},
		{"扩展名 php", "src/OrderService.php", phpSample, LangPHP},
		{"扩展名 json", "package.json", `{"name":"x"}`, LangJSON},
		{"扩展名 yaml", "deploy/app.yaml", "a: 1", LangYAML},
		{"扩展名 sql", "schema.sql", "select 1", LangSQL},
		{"扩展名 markdown", "README.md", "# 标题", LangMarkdown},
		{"扩展名 text", "notes.txt", "hello", LangText},
		{"shebang python", "scripts/run", "#!/usr/bin/env python\nprint(1)", LangPython},
		{"内容 php", "index", "<?php echo 1;", LangPHP},
		{"内容 go", "main", "package main\n\nfunc main() {\n}", LangGo},
		{"内容 react", "component", "import React from 'react';\nexport default function App(){return null}", LangJavaScript},
		{"js 内 TS 注解", "plain.js", "export function f(x: string): number { return 1; }", LangTypeScript},
		{"未知内容", "", "\x00\x01random bytes", LangText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetectLanguage(c.path, []byte(c.content)); got != c.want {
				t.Fatalf("DetectLanguage(%q) = %q, 期望 %q", c.path, got, c.want)
			}
		})
	}
	if got := DetectLanguage("x.go", nil); got != LangGo {
		t.Fatalf("空内容也应按扩展名判定，得到 %q", got)
	}
}

func TestGuessLanguageFamily(t *testing.T) {
	cases := map[string]string{
		"java":       "jvm",
		"kotlin":     "jvm",
		"go":         "golang",
		"python":     "python",
		"typescript": "node",
		"javascript": "node",
		"php":        "php",
		"rust":       "other",
		"":           "other",
	}
	for in, want := range cases {
		if got := GuessLanguageFamily(in); got != want {
			t.Fatalf("GuessLanguageFamily(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestExtractSymbols(t *testing.T) {
	cases := []struct {
		name    string
		lang    string
		content string
		want    []string
	}{
		{"go", LangGo, goSample, []string{"maxRetry", "ErrOrder", "OrderService", "NewOrderService", "Create"}},
		{"java", LangJava, javaSample, []string{"OrderService", "create", "cancel"}},
		{"python", LangPython, pythonSample, []string{"MAX_RETRY", "OrderService", "__init__", "create", "_retry", "main"}},
		{"typescript", LangTypeScript, tsSample, []string{"OrderDto", "create", "OrderService", "helper", "handler"}},
		{"php", LangPHP, phpSample, []string{"OrderService", "create"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractSymbols(c.lang, c.content)
			if len(got) == 0 {
				t.Fatalf("未提取到任何符号")
			}
			for _, w := range c.want {
				if !contains(got, w) {
					t.Fatalf("符号缺少 %q，实际=%v", w, got)
				}
			}
			// 去重校验
			seen := map[string]bool{}
			for _, s := range got {
				if seen[s] {
					t.Fatalf("符号重复: %q（%v）", s, got)
				}
				seen[s] = true
			}
		})
	}
	if got := ExtractSymbols(LangGo, ""); got != nil {
		t.Fatalf("空内容应返回 nil，得到 %v", got)
	}
}

func TestExtractImports(t *testing.T) {
	cases := []struct {
		name    string
		lang    string
		content string
		want    []string
	}{
		{"go 块与单行", LangGo, "package main\n\nimport (\n\t\"context\"\n\t\"fmt\"\n\n\t\"github.com/x/y\"\n)\n\nimport \"os\"\n", []string{"context", "fmt", "github.com/x/y", "os"}},
		{"java", LangJava, javaSample, []string{"java.util.List", "org.junit.Assert.assertNotNull"}},
		{"python", LangPython, pythonSample, []string{"os", "sys", "json", "typing"}},
		{"typescript", LangTypeScript, tsSample, []string{"@nestjs/common", "axios", "fs"}},
		{"php", LangPHP, phpSample, []string{"App\\Repository\\OrderRepository"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractImports(c.lang, c.content)
			for _, w := range c.want {
				if !contains(got, w) {
					t.Fatalf("import 缺少 %q，实际=%v", w, got)
				}
			}
			for _, g := range got {
				if strings.Contains(g, ";") || strings.Contains(g, `"`) {
					t.Fatalf("import 结果不应包含引号或分号: %q", g)
				}
			}
		})
	}
}

func TestLineCountAndSliceLines(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5"
	if got := LineCount(content); got != 5 {
		t.Fatalf("LineCount = %d, 期望 5", got)
	}
	if got := LineCount("l1\nl2\n"); got != 2 {
		t.Fatalf("末尾换行不应产生额外行，得到 %d", got)
	}
	if got := LineCount(""); got != 0 {
		t.Fatalf("空内容行数应为 0，得到 %d", got)
	}

	cases := []struct {
		start, end int
		wantText   string
		wantStart  int
		wantEnd    int
	}{
		{2, 4, "l2\nl3\nl4", 2, 4},
		{0, 2, "l1\nl2", 1, 2},
		{4, 99, "l4\nl5", 4, 5},
		{-5, 100, "l1\nl2\nl3\nl4\nl5", 1, 5},
		{5, 5, "l5", 5, 5},
	}
	for _, c := range cases {
		text, s, e := SliceLines(content, c.start, c.end)
		if text != c.wantText || s != c.wantStart || e != c.wantEnd {
			t.Fatalf("SliceLines(%d,%d) = (%q,%d,%d)，期望 (%q,%d,%d)",
				c.start, c.end, text, s, e, c.wantText, c.wantStart, c.wantEnd)
		}
	}
	if text, s, e := SliceLines(content, 9, 99); text != "" || s != 0 || e != 0 {
		t.Fatalf("完全越界应返回 (\"\",0,0)，得到 (%q,%d,%d)", text, s, e)
	}
	if text, s, e := SliceLines("", 1, 5); text != "" || s != 0 || e != 0 {
		t.Fatalf("空内容应返回 (\"\",0,0)，得到 (%q,%d,%d)", text, s, e)
	}
}

func TestFocusWindow(t *testing.T) {
	lines := make([]string, 0, 100)
	for i := 1; i <= 100; i++ {
		lines = append(lines, "line-"+itoa(i))
	}
	content := strings.Join(lines, "\n")

	snippet, start, end := FocusWindow(content, 50, 3)
	if start != 47 || end != 53 {
		t.Fatalf("FocusWindow 起止 = (%d,%d)，期望 (47,53)", start, end)
	}
	if !strings.HasPrefix(snippet, "line-47") || !strings.HasSuffix(snippet, "line-53") {
		t.Fatalf("FocusWindow 内容不符: %q", snippet)
	}
	if _, s, e := FocusWindow(content, 2, 60); s != 1 || e != 62 {
		t.Fatalf("上边界应收敛到 1，得到 (%d,%d)", s, e)
	}
	if _, s, e := FocusWindow(content, 99, 60); s != 39 || e != 100 {
		t.Fatalf("下边界应收敛到 100，得到 (%d,%d)", s, e)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestFindSnippetAndApplySnippetReplace(t *testing.T) {
	content := strings.Join([]string{
		"package demo",
		"",
		"func Add(a, b int) int {",
		"    if a == 0 {",
		"        return b",
		"    }",
		"    return a + b",
		"}",
	}, "\n")

	// 片段缩进与文件不一致时仍应命中。
	oldSnippet := "if a == 0 {\nreturn b\n}"
	line, ok := FindSnippet(content, oldSnippet, 0)
	if !ok || line != 4 {
		t.Fatalf("FindSnippet = (%d,%v)，期望 (4,true)", line, ok)
	}

	newSnippet := "if a <= 0 {\nreturn b + 1\n}"
	out, start, end, err := ApplySnippetReplace(content, oldSnippet, newSnippet, 0)
	if err != nil {
		t.Fatalf("ApplySnippetReplace 失败: %v", err)
	}
	if start != 4 || end != 6 {
		t.Fatalf("变更区间 = (%d,%d)，期望 (4,6)", start, end)
	}
	wantLines := []string{
		"package demo",
		"",
		"func Add(a, b int) int {",
		"    if a <= 0 {",
		"        return b + 1",
		"    }",
		"    return a + b",
		"}",
	}
	if out != strings.Join(wantLines, "\n") {
		t.Fatalf("替换结果不符：\n%s", out)
	}
	if LineCount(out) != LineCount(content) {
		t.Fatalf("行数发生变化：%d → %d", LineCount(content), LineCount(out))
	}

	// 找不到片段必须返回错误
	if _, _, _, err := ApplySnippetReplace(content, "完全不存在的片段", "x", 0); err == nil {
		t.Fatalf("片段缺失时应返回错误")
	}

	// fromLine 生效：片段出现在多行时只从 fromLine 之后查找
	dup := "a\nTARGET\nb\nTARGET\nc"
	if l, ok := FindSnippet(dup, "TARGET", 4); !ok || l != 4 {
		t.Fatalf("fromLine 查找失败: (%d,%v)", l, ok)
	}
	// fromLine 之后无命中时回退全文（此处 fromLine 已越过文件末尾）
	if l, ok := FindSnippet(dup, "TARGET", 99); !ok || l != 2 {
		t.Fatalf("回退全文查找失败: (%d,%v)", l, ok)
	}
	// 从中间行查找时只在 fromLine 之后命中
	if l, ok := FindSnippet(dup, "TARGET", 4); !ok || l != 4 {
		t.Fatalf("fromLine=4 应命中第 4 行: (%d,%v)", l, ok)
	}
	if _, ok := FindSnippet(content, "", 0); ok {
		t.Fatalf("空片段不应命中")
	}
}

func TestApplySnippetReplacePreservesDeeperIndent(t *testing.T) {
	content := "class A {\n    void f() {\n        run();\n    }\n}"
	old := "void f() {\nrun();\n}"
	newer := "void f() {\n  run();\n  stop();\n}"
	out, start, end, err := ApplySnippetReplace(content, old, newer, 0)
	if err != nil {
		t.Fatalf("替换失败: %v", err)
	}
	if start != 2 || end != 5 {
		t.Fatalf("区间 = (%d,%d)，期望 (2,5)（新片段 3 行，替换掉原 2 行）", start, end)
	}
	want := "class A {\n    void f() {\n      run();\n      stop();\n    }\n}"
	if out != want {
		t.Fatalf("缩进保持失败：\n得到: %q\n期望: %q", out, want)
	}
}

func TestIsBinaryAndTruncate(t *testing.T) {
	if IsBinary([]byte("hello world")) {
		t.Fatalf("纯文本不应判定为二进制")
	}
	if !IsBinary([]byte("abc\x00def")) {
		t.Fatalf("含 NUL 应判定为二进制")
	}
	if !IsBinary([]byte{0x80, 0x81, 0x82, 0xff}) {
		t.Fatalf("非法 UTF-8 应判定为二进制")
	}
	if IsBinary(nil) {
		t.Fatalf("空内容不应判定为二进制")
	}

	long := strings.Repeat("中", 100)
	cut := TruncateUTF8(long, 10)
	if len(cut) > 10 {
		t.Fatalf("TruncateUTF8 超限: %d", len(cut))
	}
	if !utf8ValidString(cut) {
		t.Fatalf("TruncateUTF8 切断了多字节字符: %q", cut)
	}
	if got := TruncateChars(long, 5); got != "中中中中中" {
		t.Fatalf("TruncateChars = %q", got)
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

func TestSortStringsStable(t *testing.T) {
	got := SortStringsStable([]string{"b", "a", "b", "", "c", "a"})
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("去重结果 = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("去重结果 = %v，期望 %v", got, want)
		}
	}
}
