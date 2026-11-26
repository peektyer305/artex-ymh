export const mentionKinds = [
  { kind: "finding", label: "ファインディング", alias: "finding" },
  { kind: "asset", label: "アセット", alias: "asset" },
  { kind: "company", label: "企業", alias: "company" },
  { kind: "endpoint", label: "エンドポイント", alias: "api" },
  { kind: "ip", label: "IP", alias: "ip" },
  { kind: "app", label: "アプリ", alias: "app" },
  { kind: "root_domain", label: "ルートドメイン", alias: "domain" },
  { kind: "subdomain", label: "サブドメイン", alias: "subdomain" },
  { kind: "service", label: "サービス", alias: "service" },
] as const;

export type MentionKind = (typeof mentionKinds)[number]["kind"];
export interface ChatMention {
  kind: MentionKind;
  id: number;
  label: string;
  description: string;
}

export function activeMention(value: string, caret: number) {
  const before = value.slice(0, caret);
  const start = before.lastIndexOf("@");
  if (start < 0 || (start > 0 && /[\w.+/-]/.test(before[start - 1]))) return null;
  const query = before.slice(start + 1);
  if (/[[\]\r\n@]/.test(query) || query.length > 220) return null;
  return { start, end: caret, query };
}

export function mentionSearch(query: string) {
  const text = query.trimStart().toLowerCase();
  for (const item of mentionKinds) {
    for (const alias of [item.label.toLowerCase(), item.alias, ...Object.keys(legacyKinds).filter((key) => legacyKinds[key] === item.kind)]) {
      if (text === alias || text.startsWith(`${alias} `) || (/[^a-z]/.test(alias) && text.startsWith(alias))) {
        return { kind: item.kind, query: query.trimStart().slice(alias.length).trim(), categories: [] };
      }
    }
  }
  const categories = mentionKinds.filter(
    (item) => item.label.toLowerCase().startsWith(text) || item.alias.startsWith(text) || Object.keys(legacyKinds).some((key) => legacyKinds[key] === item.kind && key.startsWith(text)),
  );
  return { kind: "" as const, query: query.trim(), categories };
}

const legacyKinds: Record<string, MentionKind> = {
  "漏洞": "finding", "资产": "asset", "企业": "company", "接口": "endpoint",
  "应用": "app", "主域名": "root_domain", "子域名": "subdomain", "服务": "service",
};

export function mentionToken(item: ChatMention) {
  const kind = item.kind;
  const label = item.label
    .replace(/[[\]]/g, (char) => (char === "[" ? "（" : "）"))
    .replace(/\s+/g, " ")
    .slice(0, 100);
  return `@[${kind}#${item.id} ${label}]`;
}

export function selectedMentions(value: string) {
  const result: Array<{ token: string; label: string; start: number }> = [];
  for (const match of value.matchAll(/@\[([^#\]\r\n]+)#([0-9]+)(?: ([^\]\r\n]*))?\]/g)) {
    const entry = mentionKinds.find((item) => item.kind === match[1] || item.label === match[1] || item.kind === legacyKinds[match[1]]);
    if (!entry) continue;
    result.push({ token: match[0], label: `${entry.label} #${match[2]}${match[3] ? ` · ${match[3]}` : ""}`, start: match.index });
  }
  return result;
}
