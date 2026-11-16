package intercept

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// SettingAutoAllow is the settings キー for the one-click auto-allow switch
// (「ワンクリック許可」). Value is JSON: {"enabled":bool,"expires_at":unix}. A single
// key keeps the state atomic — two keys could disagree on partial writes.
const SettingAutoAllow = "guard_auto_allow"

// AutoAllowState is the resolved auto-allow status returned by the API.
type AutoAllowState struct {
	Enabled          bool  `json:"enabled"`
	ExpiresAt        int64 `json:"expires_at"`
	RemainingSeconds int64 `json:"remaining_seconds"`
}

// autoAllowSetting is the stored form of the setting.
type autoAllowSetting struct {
	Enabled   bool  `json:"enabled"`
	ExpiresAt int64 `json:"expires_at"`
}

// autoAllowHours は {2,4,8} にクランプする：≤2 → 2、≤4 → 4、それ以外 → 8。
func autoAllowHours(hours int) int {
	switch {
	case hours <= 2:
		return 2
	case hours <= 4:
		return 4
	default:
		return 8
	}
}

// SetAutoAllow はワンクリック許可をオン/オフする。オンのとき有効期間は hours 時間（2/4/8 にクランプ）。
// オフのときは hours を無視し、失効時刻をクリアする。
func (i *Interceptor) SetAutoAllow(enabled bool, hours int) error {
	s := autoAllowSetting{}
	if enabled {
		s.Enabled = true
		s.ExpiresAt = time.Now().Add(time.Duration(autoAllowHours(hours)) * time.Hour).Unix()
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return i.db.SetSetting(SettingAutoAllow, string(raw))
}

// AutoAllowStatus は現在のワンクリック許可状態を返す。呼び出しごとに遅延失効チェックを行い、
// 失効していれば自動でオフにする（settings に書き戻してログを記録）。タイマーなしで
// 期限切れ時に自動失効するのと同じ効果になる。
func (i *Interceptor) AutoAllowStatus() AutoAllowState {
	v, ok, err := i.db.GetSetting(SettingAutoAllow)
	if err != nil || !ok {
		return AutoAllowState{}
	}
	var s autoAllowSetting
	if err := json.Unmarshal([]byte(v), &s); err != nil || !s.Enabled {
		return AutoAllowState{}
	}
	remaining := s.ExpiresAt - time.Now().Unix()
	if remaining <= 0 {
		log.Printf("[intercept] ワンクリック許可の期限が切れたため、手動承認に自動で戻します")
		if err := i.SetAutoAllow(false, 0); err != nil {
			log.Printf("[intercept] ワンクリック許可の期限切れによる自動オフに失敗しました：%v", err)
		}
		return AutoAllowState{}
	}
	return AutoAllowState{Enabled: true, ExpiresAt: s.ExpiresAt, RemainingSeconds: remaining}
}

// autoAllowApprove handles an "ask" decision while ワンクリック許可 is active: record
// the approval into history with decision_source = "auto_allow" (監査は完全で、無人での
// 許可と手動／モデル／ルールの判定を区別できる), then allow without creating a pending
// record. deny ルールはここを通らない（guard の deny 分岐が直接ブロックし、このスイッチの影響を受けない）。
func (i *Interceptor) autoAllowApprove(ctx context.Context, convID int64, dec Decision, toolName string, input []byte) {
	taskID, agentName := taskInfoFromCtx(ctx)
	audit := auditFor(ctx, dec, input, "allowed")
	reason := fmt.Sprintf("ワンクリック許可による自動承認: %s", dec.Message)
	id, err := i.db.CreateDecidedInterceptWithSource(dec.RuleID, convID, taskID, agentName, toolName, input, "allowed", reason, "auto_allow", audit)
	if err == nil {
		i.bindResult(ctx, id, audit)
	}
}
