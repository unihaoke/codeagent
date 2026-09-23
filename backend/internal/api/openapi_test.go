package handler

import (
	"testing"

	"github.com/codeagent/backend/internal/domain"
)

// TestMatchRepoLocator 校验按 git 地址/主机标识定位仓库的打分优先级。
func TestMatchRepoLocator(t *testing.T) {
	repos := []domain.Repository{
		{
			ID: "r1", Key: "order-service", Name: "订单服务", URL: "https://git.x/order.git",
			MatchRules: domain.RepoMatchRules{HostPatterns: []string{"order-svc", "order.internal"}},
		},
		{
			ID: "r2", Key: "pay-service", Name: "支付服务", URL: "https://git.x/pay.git",
			MatchRules: domain.RepoMatchRules{
				EndpointPatterns: []string{"/api/payment/**"}, Keywords: []string{"payment"},
			},
		},
	}
	cases := []struct {
		name string
		loc  *repoLocator
		want string
	}{
		{"gitUrl 精确匹配", &repoLocator{GitURL: "https://git.x/order.git"}, "r1"},
		{"主机模式命中", &repoLocator{Host: "order-svc"}, "r1"},
		{"主机片段命中 key", &repoLocator{Host: "pay"}, "r2"},
		{"关键词命中", &repoLocator{Host: "payment"}, "r2"},
		{"无匹配", &repoLocator{Host: "unknown"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			best, bestScore := "", 0
			for i := range repos {
				if sc, _ := matchRepoLocator(&repos[i], c.loc); sc > bestScore {
					bestScore, best = sc, repos[i].ID
				}
			}
			if best != c.want {
				t.Fatalf("loc=%v want=%s got=%s", c.loc, c.want, best)
			}
		})
	}
}
