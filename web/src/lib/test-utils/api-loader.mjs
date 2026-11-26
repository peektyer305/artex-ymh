import { existsSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";

const sourceRoot = new URL("../../", import.meta.url);

// Match the application's @/ alias and bundler-style extensionless imports in Node tests.
export async function resolve(specifier, context, nextResolve) {
  let target;
  if (specifier.startsWith("@/")) target = new URL(specifier.slice(2), sourceRoot);
  else if (specifier.startsWith(".") && context.parentURL?.startsWith("file:")) {
    target = new URL(specifier, context.parentURL);
  }
  if (target?.protocol === "file:") {
    const path = fileURLToPath(target);
    for (const candidate of [path + ".ts", path + "/index.ts", path + ".mjs", path + ".js"]) {
      if (existsSync(candidate)) return { url: pathToFileURL(candidate).href, shortCircuit: true };
    }
  }
  return nextResolve(specifier, context);
}
