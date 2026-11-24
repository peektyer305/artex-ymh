import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// ドロワー/ダイアログ(Sheet/Dialog)の onInteractOutside による閉じ判定の補助。
//
// 背景：ドロワー内の Radix ポップアップ（Select ドロップダウン、DropdownMenu、Popover など）は
// ドロワーの外へ portal される。開いたままマスク/外側をクリックして閉じようとすると、この
// pointerdown は Select と Sheet の両 DismissableLayer で同時に処理される。Select が先に閉じ、
// discrete イベントで React が同期 flush するため、Sheet のハンドラの番には既に data-state は
// closed になっている。「その瞬間」に開いているかを判定するのは元々当てにならない（実測済み）。
//
// 正しい方法：Radix の pointerdown 監視はバブル段階にある。そこで capture 段階（それより前）で
// 「この時点でポップアップが開いているか」を記録し、onInteractOutside がその記録値で
// 閉じてよいか判断する。
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture:Radix のバブル段階の pointerdown ハンドラより先に記録する
  );
}

// radixOverlayWasOpenAtPointerDown は「直近の pointerdown 時に Radix ポップアップが開いていたか」を返す。
// ドロワー/ダイアログは、開いている時はマスクをクリックしてもポップアップのみ閉じ、自身は閉じない。
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText はテキストをクリップボードへ書き込み、成功したかを返す。
// 背景：navigator.clipboard はセキュアコンテキスト(HTTPS / localhost)でのみ利用可能で、IP + HTTP
// でアクセスすると undefined になる。その場合は execCommand("copy") へフォールバックする。
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // フォールバックへ進む
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
