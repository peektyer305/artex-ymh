package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**脆弱性トラフィックエビデンス（任意）**：report_finding で脆弱性を報告する際、閲覧済みで脆弱性の結論を裏付けると確認した HTTP リクエスト/レスポンスがあれば、traffic_refs で再現順に実際の ID を関連付けられる。ドメインと時刻は候補の絞り込みにのみ使い、関連を推定しない。TCP などの非 HTTP 脆弱性、未収集、または確実な一致がない場合は省略するか [] を渡し、evidence にコマンド出力やログなど他の検証可能なエビデンスを残す。未関連付けの理由を説明することを推奨する。ID を推測せず、パケットを補うためだけに重複してプローブしない。"

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
