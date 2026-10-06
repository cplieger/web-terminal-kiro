import { expect, test, type Page, type Request } from "@playwright/test";

// Clipboard image paste through the served page and REAL key presses: the question
// is whether Chromium fires a native paste the page can take, which no unit test can
// answer. POST /api/uploads is mocked, so a run writes nothing on the server it is
// pointed at; the server's own write path is upload_test.go's.

const UPLOADED = "/uploads/pasted-2026-08-15T08-42-11.png";

async function bootWithClipboardImage(page: Page): Promise<Request[]> {
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  const uploads: Request[] = [];
  await page.route("**/api/uploads", async (route) => {
    uploads.push(route.request());
    await route.fulfill({ status: 200, json: { uploaded: [UPLOADED] } });
  });
  await page.goto("/", { waitUntil: "load" });
  await expect(page.locator(".term-input").first()).toBeAttached({ timeout: 15_000 });
  await page.evaluate(async () => {
    const canvas = new OffscreenCanvas(16, 16);
    const ctx = canvas.getContext("2d");
    if (ctx === null) {
      throw new Error("no 2d context");
    }
    ctx.fillRect(0, 0, 16, 16);
    const png = await canvas.convertToBlob({ type: "image/png" });
    await navigator.clipboard.write([new ClipboardItem({ "image/png": png })]);
  });
  return uploads;
}

function multipartFilename(req: Request): string | null {
  return /filename="(pasted-[^"]+)"/.exec(req.postData() ?? "")?.[1] ?? null;
}

test.describe("clipboard image paste", () => {
  for (const keys of ["Control+V", "Control+Shift+V"]) {
    test(`${keys} in the terminal uploads the clipboard image`, async ({ page }) => {
      const uploads = await bootWithClipboardImage(page);
      await page.locator(".term-input").first().focus();

      await page.keyboard.press(keys);

      await expect.poll(() => uploads.length, { timeout: 10_000 }).toBe(1);
      expect(multipartFilename(uploads[0]!)).toMatch(
        /^pasted-\d{4}-\d\d-\d\dT\d\d-\d\d-\d\d\.png$/,
      );
    });
  }

  test("Control+V with only text on the clipboard uploads nothing", async ({ page }) => {
    const uploads = await bootWithClipboardImage(page);
    await page.evaluate(() => navigator.clipboard.writeText("plain text"));
    await page.locator(".term-input").first().focus();

    await page.keyboard.press("Control+V");
    await page.waitForTimeout(1000);

    expect(uploads).toHaveLength(0);
  });

  // The type-to-focus state after a selection, where the body holds focus.
  test("Control+V with nothing focused uploads the clipboard image", async ({ page }) => {
    const uploads = await bootWithClipboardImage(page);
    await page.evaluate(() => {
      (document.activeElement as HTMLElement | null)?.blur();
    });
    await expect.poll(() => page.evaluate(() => document.activeElement?.tagName)).toBe("BODY");

    await page.keyboard.press("Control+V");

    await expect.poll(() => uploads.length, { timeout: 10_000 }).toBe(1);
  });
});
