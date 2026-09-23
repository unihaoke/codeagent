// Package store 的错误定义。
package store

import "errors"

var (
	// ErrNotFound 资源不存在。
	ErrNotFound = errors.New("资源不存在")
	// ErrConflict 唯一键冲突。
	ErrConflict = errors.New("资源已存在")
)
