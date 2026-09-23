// 本文件提供集成测试所需的适配器与 git 辅助（与 cmd/server/main.go 的装配保持一致）。
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/source"
	"github.com/codeagent/backend/internal/store"
)

// secretProvider 把凭证加密箱适配为 source.SecretProvider。
type secretProvider struct {
	st  store.Store
	box domain.CredentialBox
}

// RepoSecret 返回仓库解密后的访问凭证。
func (p *secretProvider) RepoSecret(_ context.Context, tenantID, repoID string) (string, string, error) {
	repo, ok := p.st.GetRepo(tenantID, repoID)
	if !ok || repo.CredentialID == "" {
		return "", "", nil
	}
	cred, ok := p.st.GetCredential(tenantID, repo.CredentialID)
	if !ok || cred.SecretEnc == "" {
		return "", "", nil
	}
	secret, err := p.box.Open(cred.SecretEnc)
	if err != nil {
		return "", "", fmt.Errorf("解密仓库凭证失败: %w", err)
	}
	return cred.Username, secret, nil
}

// materializer 把 source.Resolver 的物化能力适配为 sandbox.Materializer。
type materializer struct {
	resolver *source.Resolver
}

// Materialize 把指定仓库的 commit 快照检出到 dest 目录。
func (m *materializer) Materialize(ctx context.Context, repo *domain.Repository, commit, dest string) error {
	if repo == nil {
		return errors.New("仓库为空")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(dest), ".staging-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	dir, err := m.resolver.Materialize(ctx, domain.RepoRef{Repository: repo, Commit: commit}, staging)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return copyDir(dir, dest)
}

// copyDir 递归复制目录（不跟随符号链接）。
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// lookGit 探测 git 可执行文件。
func lookGit() (string, error) { return exec.LookPath("git") }

// runGit 在指定目录执行 git 命令，失败即终止测试。
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=CodeAgent Test",
		"GIT_AUTHOR_EMAIL=test@acme.internal",
		"GIT_COMMITTER_NAME=CodeAgent Test",
		"GIT_COMMITTER_EMAIL=test@acme.internal",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v 失败: %v\n%s", args, err, string(out))
	}
	return string(out)
}

// 保证日志依赖被使用（部分断言在无 git 环境下会走 logx.Nop）。
var _ = logx.Nop
