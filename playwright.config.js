import { defineConfig, devices } from "@playwright/test";

/**
 * Sandbox config for the harness architecture test.
 *
 * The load-bearing part is per-run isolation. By default Playwright clears
 * outputDir and overwrites playwright-report/ at the start of every run, which
 * is exactly the "a new run wipes the previous results" problem — and why two
 * users cannot run at the same time.
 *
 * The fix is to make all three output locations env-driven so a supervising
 * server (see server/internal/run/layout.go) can hand each run its own,
 * proven here under real concurrency against a target with nothing else in
 * the way.
 *
 * PW_HTML_REPORT_DIR must be a SIBLING of the output dir — nesting them is a
 * hard Playwright error.
 */
export default defineConfig({
  testDir: "./tests",
  fullyParallel: true,
  workers: parseInt(process.env.PW_WORKERS ?? "2", 10),
  timeout: parseInt(process.env.PW_TIMEOUT ?? "60000", 10),

  outputDir: process.env.PW_OUTPUT_DIR ?? "./test-results",

  reporter: [
    ["line"],
    ...(process.env.PW_RESULT_JSON
      ? [/** @type {any} */ (["json", { outputFile: process.env.PW_RESULT_JSON }])]
      : []),
    ["html", {
      open: "never",
      outputFolder: process.env.PW_HTML_REPORT_DIR ?? "playwright-report",
    }],
  ],

  use: {
    headless: process.env.PW_HEADLESS !== "false",
    trace: /** @type {any} */ (process.env.PW_TRACE ?? "retain-on-failure"),
    screenshot: "only-on-failure",
  },

  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
  ],
});
