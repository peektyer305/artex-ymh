import { fileURLToPath } from "node:url";

// 静的エクスポート：`NEXT_EXPORT=1 next build` で純静的ディレクトリを web/out に出力し、そのまま
// nginx の web ルートに置いて配信できる。開発時（next dev）はこの変数を設定せず、/api のリバースプロキシとホットリロードを維持する。
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel デモ：サイト全体を mock で動かし、バックエンドなし。/api のリバースプロキシも不要。
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 親ディレクトリの lockfile がルート推論やリソースパス生成に影響しないようにする。
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // イントラネット内 IP から dev リソース（HMR）へのアクセスを許可する（必要に応じて増減）。
  // dev 段階では任意の IPv4 ソースから /_next/* と HMR へのアクセスを開放する（イントラネット IP が変わっても影響なし）。
  // 注意：Next は安全上の理由から裸の "*" を禁止しているため、セグメント単位のワイルドカードを使う。"*.*.*.*" は任意の IPv4 に一致する。
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 純静的エクスポート：Node ランタイムなし。画像は最適化しない。各ルートは <route>/index.html を生成する。
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock デモ：バックエンドなし。/api のリバースプロキシも不要。
          images: { unoptimized: true },
        }
      : {
          // 開発時：/api/* を Go バックエンドへリバースプロキシする（デフォルトは :8787、AUTOPENTEST_API で上書き可能）。
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
