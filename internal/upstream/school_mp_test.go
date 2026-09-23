package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestMPEventBase(t *testing.T) {
	a := &auth.Auth{UID: "u-1", Nickname: "测试"}
	base := mpEventBase(a)
	for _, k := range []string{"ideType", "extName", "ideName", "platform", "userId"} {
		if _, ok := base[k]; !ok {
			t.Errorf("missing common field %s", k)
		}
	}
	if base["ideType"] != "WorkBuddy_MP" || base["extName"] != "workbuddy-mp" {
		t.Errorf("fingerprint ideType=%v extName=%v", base["ideType"], base["extName"])
	}
}

func TestSchoolChatTimesEvents(t *testing.T) {
	ev := SchoolChatTimesEvents("conv-1")
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	if ev["conversationId"] != "conv-1" || ev["codebuddy.session_id"] != "conv-1" {
		t.Errorf("conversation join fields missing: %v", ev)
	}
	b, _ := json.Marshal(ev)
	if !strings.Contains(string(b), "agentName") {
		t.Error("agentName missing")
	}
}

func TestSchoolExpertUseEvents(t *testing.T) {
	events := SchoolExpertUseEvents("ex_x", "论文写作导师", "conv-2")
	if len(events) != 4 {
		t.Fatalf("events=%d want 4", len(events))
	}
	wantCodes := []string{"expert_summon_click", "expert_summoned", "expert_actual_use", "chat_request_send"}
	for i, code := range wantCodes {
		if events[i]["eventCode"] != code {
			t.Errorf("events[%d].eventCode=%v want %s", i, events[i]["eventCode"], code)
		}
	}
	if events[0]["id"] != "ex_x" || events[0]["expertTitle"] != "论文写作导师" {
		t.Errorf("expert fields: %v", events[0])
	}
	if events[3]["expertId"] != "ex_x" {
		t.Errorf("chat expertId=%v", events[3]["expertId"])
	}
}

// TestMiniExpertUseEventMPFingerprint Sequential_Tasks_2 判据载体：mp 指纹
// expert_actual_use——与 school 域 SchoolExpertUseEvents 是两套口径（不带
// conversationId/activityId、extVersion=2.2.8、type=send_message）。
func TestMiniExpertUseEventMPFingerprint(t *testing.T) {
	ev := MiniExpertUseEvent("ex_real001", "论文写作导师", "agent")
	if ev["eventCode"] != "expert_actual_use" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	if ev["id"] != "ex_real001" || ev["name"] != "ex_real001" {
		t.Errorf("expert id fields: %v", ev)
	}
	if ev["expertTitle"] != "论文写作导师" || ev["expertType"] != "agent" {
		t.Errorf("expert meta fields: %v", ev)
	}
	if ev["extVersion"] != "2.2.8" || ev["source"] != "mini_program" || ev["type"] != "send_message" {
		t.Errorf("mp 指纹不符: extVersion=%v source=%v type=%v", ev["extVersion"], ev["source"], ev["type"])
	}
	if _, ok := ev["conversationId"]; ok {
		t.Error("mp 专家事件不应带 conversationId（school 域口径勿混）")
	}
	if _, ok := ev["activityId"]; ok {
		t.Error("mp 专家事件不应带 activityId（school 域口径勿混）")
	}
}

// TestMiniExpertUseEventDefaults 空 expertType/name 的兜底：type=agent、name 回落 id。
func TestMiniExpertUseEventDefaults(t *testing.T) {
	ev := MiniExpertUseEvent("ex_only_id", "", "")
	if ev["expertType"] != "agent" {
		t.Errorf("expertType=%v want agent", ev["expertType"])
	}
	if ev["expertTitle"] != "ex_only_id" {
		t.Errorf("expertTitle=%v want id 兜底", ev["expertTitle"])
	}
}

// TestMiniChatModelEvent Sequential_Tasks_5 判据载体：裸对话事件 + 模型字段。
func TestMiniChatModelEvent(t *testing.T) {
	ev := MiniChatModelEvent("conv-glm", "glm-5.2", "GLM-5.2")
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v want chat_request_send", ev["eventCode"])
	}
	if ev["requestModelId"] != "glm-5.2" || ev["requestModelName"] != "GLM-5.2" {
		t.Errorf("模型字段缺失: %v", ev)
	}
	// 与裸对话事件同形状（Tasks_1/3 口径）——conversationId JOIN 必须保留。
	if ev["conversationId"] != "conv-glm" {
		t.Errorf("conversationId=%v", ev["conversationId"])
	}
}

// TestMiniPlaybookEvents Sequential_Tasks_7 的 mp 形态判据组：cta_click → prompt_send。
func TestMiniPlaybookEvents(t *testing.T) {
	events := MiniPlaybookEvents("pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if len(events) != 2 {
		t.Fatalf("events=%d want 2", len(events))
	}
	if events[0]["eventCode"] != "playbook_cta_click" || events[1]["eventCode"] != "playbook_prompt_send" {
		t.Errorf("事件序列错误: %v / %v", events[0]["eventCode"], events[1]["eventCode"])
	}
	for i, ev := range events {
		if ev["id"] != "pm-gtm-launch-plan" || ev["extVersion"] != "2.2.8" {
			t.Errorf("events[%d] 载荷/指纹错误: %v", i, ev)
		}
	}
	if events[1]["conversationId"] == "" {
		t.Error("playbook_prompt_send 必须带 conversationId（JOIN 会话）")
	}
}
