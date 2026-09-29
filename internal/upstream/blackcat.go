// blackcat.go 夜猫子任务（black_cat）+ 新手礼包/补偿 API。
//
// 判据（WorkBuddy-Daily 项目实测口径 + 本网关验证）：black_cat 要求在
// **23:00–08:00（本地时区）窗口内**完成 3 次 glm-5.2 对话并上报 chat 事件链；
// 窗口外行为不计分。真实对话走网关既有 ChatStream（glm-5.2），事件链用
// ReportChatActivityModel（chat_5 同款上报形状）。
package upstream

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// InNightWindow 当前是否处于夜猫子计数窗口（23:00–08:00 本地时区）。
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// BlackcatNeed 查 black_cat 任务剩余差额（需要再完成几次对话）。
// 任务不存在返回 0（无可做）；拉取失败返回错误。
func (c *Client) BlackcatNeed(a *auth.Auth) (int64, error) {
	tasks, err := c.ListTasks(a)
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if t.TaskCode == "black_cat" {
			if t.Claimed || t.Current >= t.Target {
				return 0, nil
			}
			return t.Target - t.Current, nil
		}
	}
	return 0, nil
}

// nightChatPrompts 夜猫子任务的对话文案池。
//
// 为什么必须**多样化**（2026-09-28 线上事故的根因，不只是分类问题）：
// 此前每号每晚发**一字不差**的同一句话（"1+1等于几？直接回答。"），且不带 system、
// 不走出站改写链。大量账号在凌晨从同一出口 IP 发出**完全相同的裸文本**请求——这正是
// 内容审核最容易命中的机器指纹，一夜触发 219 次审核拒绝；而那些拒绝又被错误分类成
// 「账号封禁」，最终误禁 32 个凭证/余额完好的健康账号。
//
// 修法两层：①分类分野（见 client.go 的 contentReviewMarkers）保证误判不再永久禁号；
// ②这里治本——文案按 (uid, 日期, 第几次) 确定性散列选取，使每个账号每天每次的请求
// 文本都不同（同一账号同日重跑可复现，便于排查），并补上 system 消息使请求具备正常
// 会话形态。
//
// 注：本仓库的 ChatStream 内部**本就**会走 prepareBody 出站改写链，故这里不存在
// "绕过出站链"的问题（上游某些 fork 的形态不同）——本修复只针对"全池一字不差的
// 裸问题文本 + 无 system"这一真实指纹。
var nightChatPrompts = []string{
	"今天适合做点什么小项目？给个建议。",
	"帮我想三个周末放松的办法。",
	"用一句话介绍你自己。",
	"推荐一本值得读的书，并说明理由。",
	"如果只能学一门新技能，你推荐什么？",
	"解释一下为什么天空是蓝色的。",
	"给我一个简单的番茄炒蛋做法。",
	"列举两种提高专注力的方法。",
	"写一句鼓励人的话。",
	"解释什么是复利，要简单。",
	"推荐一个适合新手的运动。",
	"如果你能去任何地方旅行，会选哪里？",
	"说说你最喜欢的一种天气和原因。",
	"帮我把这段话改得更简洁：今天天气很好我们出去走走吧。",
	"用一个比喻解释什么是时间。",
	"教我一句实用的英语口语。",
}

// nightChatPrompt 按 (uid, 日期, 第几次) 确定性选取文案（FNV-1a 散列）。
//
// 确定性（而非每次随机）是刻意的：同一账号同一天重跑得到同一序列，便于复现排查；
// 而不同账号/不同日期/不同次数会散开到池内不同文案，消除"全池一字不差"的指纹。
func nightChatPrompt(uid, day string, nth int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(uid))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(day))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(strconv.Itoa(nth)))
	return nightChatPrompts[h.Sum64()%uint64(len(nightChatPrompts))]
}

// RunNightChats 夜猫子：发 need 次 glm-5.2 真实对话（读干流）并上报事件链。
// 返回成功次数。对话内容极短，消耗可忽略。
//
// 请求形态对齐真实客户端：带 system 消息 + 文案按 (账号,日期,次数) 散列取自池。
// （出站改写链无需在此手动调用：ChatStream 内部已走 prepareBody——指纹脱敏/档位
// 归一/角色归一/工具配对都在那里统一施加，此处再调一次只会重复劳动。）
func (c *Client) RunNightChats(a *auth.Auth, need int) (int64, error) {
	var ok int64
	day := time.Now().Format("2006-01-02")
	for i := 0; i < need; i++ {
		body, _ := json.Marshal(map[string]any{
			"model": "glm-5.2",
			"messages": []map[string]any{
				{"role": "system", "content": "You are a helpful assistant."},
				{"role": "user", "content": nightChatPrompt(a.UID, day, i)},
			},
			"stream": true,
		})
		rc, status, respBody, err := c.ChatStream(a, body, "", ChatMeta{})
		if err != nil || status >= 400 {
			if rc != nil {
				rc.Close()
			}
			return ok, fmt.Errorf("第 %d 次对话失败: http=%d err=%v body=%.120s", i+1, status, err, respBody)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		if err := c.ReportChatActivityModel(a, fmt.Sprintf("wb2api-night-%d-%d", time.Now().UnixMilli(), i), "", "glm-5.2", "GLM-5.2"); err != nil {
			return ok, fmt.Errorf("第 %d 次上报失败: %w", i+1, err)
		}
		ok++
		time.Sleep(4 * time.Second)
	}
	return ok, nil
}

// ClaimGift 领取新手礼包（每号一次，已领返回业务错误）。
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-gift", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误）。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-compensation", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// HeatmapYesterdayMissed 检查昨日是否漏签（heatmap cell score==0）。
//
// 昨日 = CST（UTC+8）自然日：必须**先 In(softRateResetLoc) 再 AddDate**——AddDate 按
// 入参 Time 所在时区做日历日减法，容器时区含夏令时时切换日的 23h/25h 会把瞬时点挪
// 1 小时、CST 日期错位一天（与 scheduler.travelDay 的「先转 CST 再取日」同口径）。
func (c *Client) HeatmapYesterdayMissed(a *auth.Auth) (bool, error) {
	yesterday := time.Now().In(softRateResetLoc).AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growthJSON(a, http.MethodGet, "/activity/growth/heatmap", nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard 对指定日期使用补签卡（保住连登连续天数；无卡返回业务错误）。
func (c *Client) UseMakeupCard(a *auth.Auth, date string) error {
	_, err := c.growthJSON(a, http.MethodPost, "/activity/growth/makeup-cards/use",
		map[string]any{"target_date": date})
	return err
}
