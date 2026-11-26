// Domain types. Cross-domain dependencies are type-only.

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 企業の新規作成時に送信する構造化アセット範囲ルール。
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// アセット範囲書き込みの結果。errors は今回の送信で不正だった行；warnings は今回の送信とは無関係だが、
// 所属結果を想定外にする既存データの問題（例: ip フィールドにホスト名が入ったアセット）。
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 企業アセット範囲ルールの 1 件（所属の唯一の正となる情報源）。
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain のとき値あり
  net?: string; // kind=ip|cidr のとき値あり
  value?: string; // kind=icp|keyword のときはバックエンドが直接返す場合あり
  raw: string; // 元のユーザー入力、表示と再入力に使用
  reason?: string;
}

// 企業：type=company のアセットノード + アイコン + アセット数 + アセット範囲ルール。
export interface Company {
  id: number;
  name: string;
  logo?: string; // リモートアイコン URL；空ならフロントは名前の頭文字を使用
  asset_count: number;
  scope?: ScopeRow[];
}
