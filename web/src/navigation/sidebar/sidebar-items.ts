import {
  Activity,
  Bot,
  Brain,
  Bug,
  ClipboardList,
  FolderOpen,
  FolderSync,
  LayoutDashboard,
  type LucideIcon,
  MessageSquare,
  Network,
  Plug,
  Radar,
  Radio,
  ScrollText,
  Settings2,
  ShieldAlert,
  Sparkles,
  Target,
  Terminal,
  Wrench,
} from "lucide-react";

export type NavBadge = "new" | "soon";

export interface NavSubItem {
  id: string;
  title: string;
  url: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

interface NavItemBase {
  id: string;
  title: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

export interface NavMainLinkItem extends NavItemBase {
  url: string;
  subItems?: never;
}

export interface NavMainParentItem extends NavItemBase {
  subItems: NavSubItem[];
}

export type NavMainItem = NavMainLinkItem | NavMainParentItem;

export interface NavGroup {
  id: number;
  label?: string;
  items: NavMainItem[];
}

export const sidebarItems: NavGroup[] = [
  {
    id: 1,
    label: "機能",
    items: [
      { id: "dashboard", title: "ダッシュボード", url: "/dashboard", icon: LayoutDashboard },
      { id: "chat", title: "チャット", url: "/chat", icon: MessageSquare },
      { id: "tasks", title: "タスク", url: "/function/tasks", icon: Target },
      { id: "findings", title: "ファインディング", url: "/function/findings", icon: Bug },
      { id: "traffic", title: "トラフィック", url: "/function/traffic", icon: Activity },
      { id: "commands", title: "ツール実行", url: "/function/commands", icon: Terminal },
      { id: "llm-records", title: "LLM レコード", url: "/function/llm-records", icon: Radio },
      { id: "assets", title: "アセット", url: "/function/assets", icon: Network },
      { id: "sync", title: "アセット同期", url: "/function/sync", icon: FolderSync },
      { id: "workspace", title: "ワークスペース", url: "/function/workspace", icon: FolderOpen },
      { id: "intranet", title: "イントラネット", url: "/intranet", icon: Radar },
    ],
  },
  {
    id: 2,
    label: "システム",
    items: [
      { id: "llm", title: "LLM", url: "/system/llm", icon: Brain },
      { id: "agents", title: "Agent", url: "/system/agents", icon: Bot },
      { id: "mcp", title: "MCP", url: "/system/mcp", icon: Plug },
      { id: "skills", title: "Skill", url: "/system/skills", icon: Sparkles },
      { id: "tools", title: "ツール", url: "/system/tools", icon: Wrench },
      { id: "intercept", title: "インターセプトルール", url: "/system/intercept", icon: ShieldAlert },
      { id: "approvals", title: "承認記録", url: "/system/intercept/approvals", icon: ClipboardList },
      { id: "logs", title: "ログ", url: "/system/logs", icon: ScrollText },
      { id: "settings", title: "システム設定", url: "/system/settings", icon: Settings2 },
    ],
  },
];
