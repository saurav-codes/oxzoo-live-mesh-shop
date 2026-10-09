// Builds the shop page into dist/ with Bun's bundler.
import { copyFileSync, mkdirSync, rmSync } from "node:fs";

rmSync("dist", { recursive: true, force: true });
mkdirSync("dist", { recursive: true });
const result = await Bun.build({ entrypoints: ["src/app.ts"], outdir: "dist", minify: true });
if (!result.success) {
  console.error(result.logs);
  process.exit(1);
}
copyFileSync("src/index.html", "dist/index.html");
console.log("built web/dist");
