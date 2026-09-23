package main

// ---------------------------------------------------------------------------
// 共享存储驱动注册点
//
// 默认构建不引入任何 database/sql 驱动（保持零依赖）：driver=sql 时需在编译期
// 通过 build tag 引入对应驱动，例如：
//
//	go build -tags pg     ./cmd/server   # PostgreSQL（github.com/lib/pq）
//	go build -tags mysql  ./cmd/server   # MySQL（github.com/go-sql-driver/mysql）
//	go build -tags sqlite ./cmd/server   # SQLite（modernc.org/sqlite，纯 Go 无 cgo）
//
// 启动参数示例：
//
//	STORE_DRIVER=sql STORE_SQL_DRIVER=postgres STORE_DSN="postgres://user:pwd@host:5432/codeagent?sslmode=disable"
// ---------------------------------------------------------------------------
