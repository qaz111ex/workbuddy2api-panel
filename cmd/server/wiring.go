package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。显式前缀 → 该域集合（跨域回落场景下粘性号在切换域后
// 由 handler 侧 realmAllowed 校验/解绑）；裸名且开启跨域回落 → 不限域（粘性可绑定任一域
// 的同模型账号，避免"裸名绑了 global 号第二天就被判不可用"）。
func realmAwareAvailableForModel(p *pool.Pool, realmFallback bool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		if realmFallback && !server.HasRealmPrefix(model) {
			return p.AvailableUIDsForModel(bare)
		}
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}
