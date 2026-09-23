package env

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/store"
)

// 引导租户标识。
//
// 这里保留的只是**登录所必需的最小引导数据**（一个租户），
// 原先随启动注入的演示仓库、演示分组、演示 Git 凭证、演示接入密钥、
// 本地演示镜像（demo-mirrors / demo-repos.json）均已移除：
// 真实数据请通过控制台「租户 / 仓库 / 分组 / 凭证 / 接入密钥」自行录入。
const (
	// BootstrapTenantID 默认租户 ID（控制台登录 tenantKey 使用该值）。
	BootstrapTenantID = "t-demo"
	// BootstrapTenantName 默认租户名。
	BootstrapTenantName = "demo"
	// BootstrapTenantDesc 默认租户描述。
	BootstrapTenantDesc = "默认租户"
)

// SeedResult 引导结果。
type SeedResult struct {
	// TenantID 引导租户 ID。
	TenantID string
	// Created 本次调用是否创建了租户（重复调用为 false）。
	Created bool
}

// EnsureSeed 幂等确保默认租户存在。
//
// 仅做一件事：没有租户时补建一个，否则原样返回。
// 控制台登录后即可自行创建租户、仓库、分组、凭证与接入密钥。
func EnsureSeed(ctx context.Context, st store.Store) (*SeedResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errors.New("env: store 不能为空")
	}

	res := &SeedResult{TenantID: BootstrapTenantID}
	created, err := ensureTenant(st, &domain.Tenant{
		ID:          BootstrapTenantID,
		Name:        BootstrapTenantName,
		Status:      domain.TenantActive,
		Description: BootstrapTenantDesc,
		Quota:       domain.DefaultQuota(),
	}, time.Now())
	if err != nil {
		return nil, err
	}
	res.Created = created
	if created {
		logx.Info("已初始化默认租户", "tenant", BootstrapTenantID)
	}
	return res, nil
}

// ensureTenant 确保租户存在（先查后建，重复调用幂等）。
func ensureTenant(st store.Store, t *domain.Tenant, now time.Time) (bool, error) {
	if _, ok := st.GetTenant(t.ID); ok {
		return false, nil
	}
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = domain.TenantActive
	}
	if err := st.CreateTenant(t); err != nil {
		return false, fmt.Errorf("创建租户 %s 失败: %w", t.ID, err)
	}
	return true, nil
}
