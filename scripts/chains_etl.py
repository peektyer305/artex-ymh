#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""chains-20260701 自問型思考チェーンのコーパス ETL（CHAINS-INTEGRATION-DESIGN.md §6 ステップ 1）。

chains-20260701/chains/ 配下の 62 個の md を解析する（3 階層構造：`# ドメイン · テーマ` → `### シナリオ文`
→ 1 行の `**Qn**:`/`**An**:` ペア。番号は節内で振り直し）。構造化 JSON を出力する：

  chains-20260701/dist/chains.json          全レコード + stats
  chains-20260701/dist/<category>.json      カテゴリ別ファイル（web/ad/app/priv/pwn/recon/rev/pivot/meta）
  chains-20260701/dist/check_report.txt     検証レポート（--check 時は表示のみで書き出さない）

レコードのフィールド：{id, category, file, section, section_idx, q_idx, q, a[, platform_specific]}
id は安定した複合キー：<ファイル名>#s<節番号>#q<節内番号>。

使い方：
  python scripts/chains_etl.py            # 解析 + 検証 + dist/ へ書き出し
  python scripts/chains_etl.py --check    # 検証のみ、レポートを表示。問題があれば終了コード 1
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "chains-20260701" / "chains"
DIST = ROOT / "chains-20260701" / "dist"

H1_RE = re.compile(r"^# (.+?)\s*$")
SECTION_RE = re.compile(r"^### (.+?)\s*$")
QA_RE = re.compile(r"^\*\*([QA])(\d+)\*\*:\s?(.*)$")
BOLD_QA_LIKE_RE = re.compile(r"^\*\*[QA]")  # 上記の形式に似ているが一致しない異常行

# 手動マーク：他プラットフォームの仕組み（ブラックボードの remember/recall、Ralph 圧縮）に結合した項目。
# (file, section_idx は 1 始まり) -> その節の全 QA に platform_specific: true を付ける
PLATFORM_SPECIFIC = {
    # 節全体が「入場券をブラックボードへ remember / recall で取り戻す / Ralph 圧縮リセット」を中心としており、
    # 外部記憶プラットフォームに依存した仕組みで、汎用の判定基準ではない。
    ("meta-close-the-kill.md", 2): "ブラックボードの remember/recall + Ralph 圧縮への結合",
}


def parse_file(path: Path):
    """(title, sections, anomalies) を返す。sections: [{title, lines:[(kind,num,text,line_no)]}]"""
    title = ""
    sections = []
    anomalies = []  # (line_no, kind, text)
    cur = None
    for no, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = raw.rstrip()
        if not line.strip():
            continue
        m = H1_RE.match(line)
        if m and not line.startswith("##"):
            if not title:
                title = m.group(1)
                continue
        m = SECTION_RE.match(line)
        if m:
            cur = {"title": m.group(1), "lines": []}
            sections.append(cur)
            continue
        if line.startswith(">"):
            continue  # tags 行などのメタ情報
        if re.fullmatch(r"-{3,}\s*", line):
            continue  # 区切り線
        m = QA_RE.match(line)
        if m:
            if cur is None:
                anomalies.append((no, "qa_outside_section", line[:80]))
                continue
            cur["lines"].append((m.group(1), int(m.group(2)), m.group(3), no))
            continue
        if BOLD_QA_LIKE_RE.match(line):
            anomalies.append((no, "malformed_qa_line", line[:80]))
            continue
        # それ以外の行（## 見出し、通常の段落など）は異常候補として記録し、人間が確認できるようにする
        anomalies.append((no, "unrecognized_line", line[:80]))
    return title, sections, anomalies


def build_records(files):
    records, report = [], []
    for path in files:
        fname = path.name
        category = fname.split("-", 1)[0]
        title, sections, anomalies = parse_file(path)
        for no, kind, text in anomalies:
            report.append(f"[異常行] {fname}:{no} {kind}: {text}")
        if not title:
            report.append(f"[ファイル] {fname}: H1 見出しがありません")
        if not sections:
            report.append(f"[ファイル] {fname}: ### シナリオ節が 1 つもありません")
        for s_idx, sec in enumerate(sections, 1):
            if not sec["lines"]:
                report.append(f"[空節] {fname}#s{s_idx} 「{sec['title']}」 に Q/A がありません")
                continue
            qs = [(n, t) for k, n, t, _ in sec["lines"] if k == "Q"]
            ans = {n: t for k, n, t, _ in sec["lines"] if k == "A"}
            # 番号の連続性：節内で振り直し、Q 番号は 1..len(qs) であるべき
            q_nums = [n for n, _ in qs]
            if q_nums != list(range(1, len(qs) + 1)):
                report.append(
                    f"[番号不連続] {fname}#s{s_idx} 「{sec['title']}」 Q 番号列 {q_nums}"
                )
            a_nums = sorted(ans)
            if a_nums != list(range(1, len(a_nums) + 1)):
                report.append(
                    f"[番号不連続] {fname}#s{s_idx} 「{sec['title']}」 A 番号列 {a_nums}"
                )
            # ペア検証
            for n, qtext in qs:
                if n not in ans:
                    report.append(
                        f"[ペア不成立] {fname}#s{s_idx}#q{n}: Q はあるが A がない 「{qtext[:40]}」"
                    )
            for n in a_nums:
                if n not in q_nums:
                    report.append(
                        f"[ペア不成立] {fname}#s{s_idx}: A{n} はあるが対応する Q がない"
                    )
            ps_key = (fname, s_idx)
            for q_idx, (n, qtext) in enumerate(qs, 1):
                rec = {
                    "id": f"{fname}#s{s_idx}#q{q_idx}",
                    "category": category,
                    "file": fname,
                    "section": sec["title"],
                    "section_idx": s_idx,
                    "q_idx": q_idx,
                    "q": qtext,
                    "a": ans.get(n, ""),
                }
                if ps_key in PLATFORM_SPECIFIC:
                    rec["platform_specific"] = True
                    rec["platform_specific_note"] = PLATFORM_SPECIFIC[ps_key]
                records.append(rec)
    return records, report


def make_stats(records, files):
    cats = {}
    for f in files:
        c = f.name.split("-", 1)[0]
        cats.setdefault(c, {"files": 0, "sections": 0, "qas": 0})
        cats[c]["files"] += 1
    seen_sections = set()
    for r in records:
        c = cats[r["category"]]
        key = (r["file"], r["section_idx"])
        if key not in seen_sections:
            seen_sections.add(key)
            c["sections"] += 1
        c["qas"] += 1
    total = {
        "files": sum(c["files"] for c in cats.values()),
        "sections": sum(c["sections"] for c in cats.values()),
        "qas": sum(c["qas"] for c in cats.values()),
    }
    return {"by_category": cats, "total": total}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--check", action="store_true", help="検証のみ行い、レポートを表示する。dist/ には書き出さない")
    ap.add_argument("--src", type=Path, default=SRC)
    ap.add_argument("--dist", type=Path, default=DIST)
    args = ap.parse_args()

    files = sorted(args.src.glob("*.md"))
    if not files:
        print(f"コーパスファイルが見つかりません：{args.src}", file=sys.stderr)
        return 2

    records, report = build_records(files)
    stats = make_stats(records, files)

    lines = ["== chains ETL 検証レポート ==", ""]
    for cat in sorted(stats["by_category"]):
        c = stats["by_category"][cat]
        lines.append(f"{cat:6s} ファイル {c['files']:2d}  節 {c['sections']:4d}  QA {c['qas']:5d}")
    t = stats["total"]
    lines.append(f"{'合計':6s} ファイル {t['files']:2d}  節 {t['sections']:4d}  QA {t['qas']:5d}")
    lines.append("")
    ps = sum(1 for r in records if r.get("platform_specific"))
    lines.append(f"platform_specific マーク：{ps} 件")
    lines.append("")
    if report:
        lines.append(f"問題を {len(report)} 件検出：")
        lines.extend("  " + r for r in report)
    else:
        lines.append("問題なし：Q/A はすべてペア、節内番号は連続、空節や異常行もなし。")
    output = "\n".join(lines)
    print(output)

    if args.check:
        return 1 if report else 0

    args.dist.mkdir(parents=True, exist_ok=True)
    payload = {"stats": stats, "records": records}
    (args.dist / "chains.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=1), encoding="utf-8"
    )
    for cat in sorted(stats["by_category"]):
        recs = [r for r in records if r["category"] == cat]
        sub = {
            "category": cat,
            "stats": stats["by_category"][cat],
            "records": recs,
        }
        (args.dist / f"{cat}.json").write_text(
            json.dumps(sub, ensure_ascii=False, indent=1), encoding="utf-8"
        )
    (args.dist / "check_report.txt").write_text(output + "\n", encoding="utf-8")
    print(f"\n書き出しました：{args.dist / 'chains.json'} + {len(stats['by_category'])} 個のカテゴリ別ファイル + check_report.txt")
    return 1 if report else 0


if __name__ == "__main__":
    sys.exit(main())
