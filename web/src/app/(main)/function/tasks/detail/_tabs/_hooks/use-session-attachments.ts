"use client";
import * as React from "react";

import { toast } from "sonner";

import { api } from "@/lib/api";
import type { ChatAttachment } from "@/lib/types";

export function useSessionAttachments(taskId: string) {
  // 方式 1 ファイルアップロード:選択済みの添付（タスク作業ディレクトリ uploads/ に保存済み）を次のメッセージと一緒に送る。
  const [attachments, setAttachments] = React.useState<ChatAttachment[]>([]);

  const [uploading, setUploading] = React.useState(false);

  const fileInputRef = React.useRef<HTMLInputElement>(null);

  async function pickFiles(files: FileList | null) {
    if (!files || files.length === 0) return;
    setUploading(true);
    try {
      const r = await api.chatUpload("task", taskId, Array.from(files));
      setAttachments((prev) => [...prev, ...r.attachments]);
    } catch (e) {
      toast.error(`アップロードに失敗しました：${(e as Error).message}`);
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  }
  return { attachments, setAttachments, uploading, setUploading, fileInputRef, pickFiles };
}
