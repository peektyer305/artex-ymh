# ARTEX-ymh 武器庫 一括インストール（旧版 → 統合先を参照）

この文書は [武器庫セットアップガイド](artex-ymh-武器庫セットアップガイド.md) に統合されました。

一括スクリプト・手動配置・よくあるつまずき・doctor の読み方は、すべて以下を参照してください。

[artex-ymh-武器庫セットアップガイド.md](artex-ymh-武器庫セットアップガイド.md)

対応スクリプトファイル: [artex-ymh-install-arsenal.sh](artex-ymh-install-arsenal.sh)

---

## クイックスタート（再掲）

```bash
# 1. スクリプト作成（ガイドの「手順 1」の内容を貼り付け）
cat > ~/install-arsenal.sh <<'SCRIPT'
# ...ガイド内のスクリプト全文...
SCRIPT
chmod +x ~/install-arsenal.sh

# 2. 実行（中国以外は PIP_INDEX=https://pypi.org/simple を設定）
~/install-arsenal.sh ~/Desktop/artex-ymh

# 3. 確認
docker compose exec artex /app/artex doctor
```
