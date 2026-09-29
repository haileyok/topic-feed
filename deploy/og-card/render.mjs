// Renders og.html (the link-preview card for feeds.hailey.at) to
// internal/feedgen/web/static/og.png at 1200x630.
//
//   cd deploy/og-card
//   npm install --no-save playwright && npx playwright install chromium
//   node render.mjs
import { chromium } from "playwright";
import { fileURLToPath, pathToFileURL } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const out = path.join(here, "../../internal/feedgen/web/static/og.png");
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
await page.goto(pathToFileURL(path.join(here, "og.html")).href);
await page.waitForTimeout(300);
await page.screenshot({ path: out, type: "png" });
await browser.close();
console.log("wrote", out);
