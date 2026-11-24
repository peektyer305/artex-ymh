"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// チャット入力欄の送信/改行キー。フロントのみの設定で、localStorage にだけ書き、DB にも
// アカウント同期にも載らないため、ブラウザを変えたら再設定が必要。issue #39 参照。0.3.2 で
// Ctrl+Enter 送信を Enter 送信に変えたので、ここで旧キーを選択肢として戻す。
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter で送信、Shift+Enter で改行" },
  { value: "ctrl-enter", label: "Ctrl+Enter で送信、Enter で改行" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// 同一タブ内の購読者集合。localStorage の storage イベントは「他の」タブでしか発火せず、
// 本ページで設定を変えた後は emit で同ページの入力欄に通知しないと、更新するまで反映されない。
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// 戻り値は文字列リテラルで、Object.is が値で比較するため useSyncExternalStore はループしない。
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// サーバー側に localStorage はないので、まず既定値を描画し、hydrate 後に getSnapshot で補正する。
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey は 1 回のキー入力で送信すべきかを判定する。
// isComposing / keyCode 229 は中国語などの IME 変換中なので必ず通す。通さないと確定の Enter で誤送信する。
// enter モードは Shift だけを除外し、0.3.2 の挙動と一字一句同じ。設定を変えないユーザーの操作感を保つ。
// ctrl-enter モードは Ctrl と Cmd（macOS）の両方を受け付ける。
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
