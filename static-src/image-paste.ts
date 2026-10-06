import type { TerminalContext, TerminalFeature } from "@cplieger/web-terminal-ui";

// Clipboard image paste: an image on the clipboard is uploaded to the container and its
// absolute path is typed at the prompt, where kiro-cli attaches it on submit. A paste with
// no image is never touched, so text paste stays exactly the UI library's.
//
// The upload policy mirrors upload.go; TestUploadPolicyMatchesClient fails on drift.

const UPLOAD_PATH = "/api/uploads";
const UPLOADS_DIR = "/uploads";
const MAX_UPLOAD_BYTES = 256 * 1024 * 1024;
const MAX_UPLOAD_FILES = 25;

/** Headroom under the server's body cap for the multipart framing around the files. */
const MULTIPART_RESERVE_BYTES = 1024 * 1024;
const UPLOAD_TIMEOUT_MS = 120_000;

/** Long-edge ceiling in pixels. A vision model is billed by pixels, so a 5000 px
 *  screenshot costs tokens without telling it more; 2000 px stays legible for UI review. */
const MAX_EDGE = 2000;

/** Types kept through a re-encode; anything else becomes PNG. The server accepts only
 *  the three extensions these map to. */
const KEEP_TYPES = new Set(["image/png", "image/jpeg", "image/webp"]);

/** Every path the server may hand back: its own name allowlist under UPLOADS_DIR. Checked
 *  so a malformed response can never put a control byte on the PTY. */
const UPLOADED_PATH = new RegExp(`^${UPLOADS_DIR}/pasted-[0-9A-Za-z.-]+$`);

export const TOAST_TOO_LARGE = "Image too large to paste";
export const TOAST_FAILED = "Image upload failed";
export const TOAST_TAB_CHANGED = "Image uploaded but not inserted: the tab changed";
// The UI clipboard feature's own wording, so Ctrl+Shift+V fails the way it always has.
const TOAST_CLIPBOARD_UNAVAILABLE = "Clipboard unavailable";
const TOAST_PASTE_BLOCKED = "Paste blocked";

function extFor(mime: string): string {
  switch (mime) {
    case "image/jpeg":
      return ".jpg";
    case "image/webp":
      return ".webp";
    default:
      return ".png";
  }
}

/** marotte's pasted-image name: a UTC second-resolution stamp, colon-free. The second and
 *  later images of one paste get `-2`, `-3`, … so they do not replace each other. */
export function pastedImageName(mime: string, now: Date = new Date(), index = 0): string {
  const stamp = now.toISOString().slice(0, 19).replace(/:/g, "-");
  const suffix = index > 0 ? `-${String(index + 1)}` : "";
  return `pasted-${stamp}${suffix}${extFor(mime)}`;
}

/** The image files a paste carries, in order; empty for a text paste. */
function clipboardImages(dt: DataTransfer | null): File[] {
  if (dt === null) {
    return [];
  }
  return Array.from(dt.files).filter((f) => f.type.startsWith("image/"));
}

/** Re-encode an image over MAX_EDGE, or of a type the server will not accept. On ANY
 *  failure the original is returned: losing the paste is worse than a large image, and
 *  prepareForUpload refuses an original whose type is not kept. */
export async function downscaleImage(file: Blob): Promise<Blob> {
  try {
    if (typeof createImageBitmap !== "function" || typeof OffscreenCanvas !== "function") {
      return file;
    }
    const bmp = await createImageBitmap(file);
    const longEdge = Math.max(bmp.width, bmp.height);
    const keep = KEEP_TYPES.has(file.type);
    if (longEdge <= MAX_EDGE && keep) {
      bmp.close();
      return file;
    }
    const scale = Math.min(1, MAX_EDGE / longEdge);
    const w = Math.max(1, Math.round(bmp.width * scale));
    const h = Math.max(1, Math.round(bmp.height * scale));
    const canvas = new OffscreenCanvas(w, h);
    const ctx = canvas.getContext("2d");
    if (ctx === null) {
      bmp.close();
      return file;
    }
    ctx.drawImage(bmp, 0, 0, w, h);
    bmp.close();
    return await canvas.convertToBlob({ type: keep ? file.type : "image/png" });
  } catch {
    return file;
  }
}

/** Downscale, then name each image from its ACTUAL type (Safari's convertToBlob answers
 *  PNG for a webp request), so the extension always matches the bytes it names. Null when
 *  an image could not be converted to a kept type: a `.png` name over TIFF bytes would be
 *  typed as a path kiro-cli then declines to attach. */
export async function prepareForUpload(
  files: readonly Blob[],
  now: Date = new Date(),
): Promise<File[] | null> {
  const out: File[] = [];
  for (const [i, f] of files.entries()) {
    const scaled = await downscaleImage(f);
    if (!KEEP_TYPES.has(scaled.type)) {
      return null;
    }
    out.push(new File([scaled], pastedImageName(scaled.type, now, i), { type: scaled.type }));
  }
  return out;
}

function skippedNotice(count: number): string {
  const noun = count === 1 ? "image" : "images";
  return `Skipped ${String(count)} ${noun}: over ${String(MAX_UPLOAD_FILES)} at once`;
}

/** An upload the server refused or that never completed; status 0 means no response. */
class UploadError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "UploadError";
    this.status = status;
  }
}

function uploadedPaths(body: unknown, count: number): string[] | null {
  if (typeof body !== "object" || body === null) {
    return null;
  }
  const list = (body as Record<string, unknown>)["uploaded"];
  if (!Array.isArray(list) || list.length !== count) {
    return null;
  }
  const paths: string[] = [];
  for (const p of list as unknown[]) {
    if (typeof p !== "string" || !UPLOADED_PATH.test(p)) {
      return null;
    }
    paths.push(p);
  }
  return paths;
}

/** POST the files and return the absolute paths the server wrote them to. */
async function uploadImages(files: readonly File[], signal: AbortSignal): Promise<string[]> {
  const total = files.reduce((n, f) => n + f.size, 0);
  if (total > MAX_UPLOAD_BYTES - MULTIPART_RESERVE_BYTES) {
    throw new UploadError(413, "image too large");
  }
  const form = new FormData();
  for (const f of files) {
    form.append("files", f, f.name);
  }
  const res = await fetch(UPLOAD_PATH, {
    method: "POST",
    body: form,
    credentials: "same-origin",
    signal: AbortSignal.any([signal, AbortSignal.timeout(UPLOAD_TIMEOUT_MS)]),
  });
  if (!res.ok) {
    throw new UploadError(res.status, `upload refused: ${String(res.status)}`);
  }
  const paths = uploadedPaths(await res.json(), files.length);
  if (paths === null) {
    throw new UploadError(res.status, "malformed upload response");
  }
  return paths;
}

function isCtrlShiftV(ev: KeyboardEvent): boolean {
  return ev.code === "KeyV" && ev.ctrlKey && ev.shiftKey && !ev.altKey && !ev.metaKey;
}

/** The terminal feature. Pane-scoped and listed BEFORE the UI preset, so its Ctrl+Shift+V
 *  handler runs ahead of the clipboard feature's text-only one (first true wins). */
export function imagePaste(): TerminalFeature {
  return {
    name: "image-paste",
    setup(ctx: TerminalContext) {
      const surface = ctx.surface();
      const doc = surface.ownerDocument;
      const life = new AbortController();
      ctx.defer(() => {
        life.abort();
      });

      function selectedPane(): boolean {
        return (
          ctx.shell.panes().find((p) => p.root.contains(surface))?.side === ctx.shell.selected()
        );
      }

      async function insertImages(images: readonly Blob[]): Promise<void> {
        const session = ctx.session.id;
        let paths: string[];
        try {
          const files = await prepareForUpload(images.slice(0, MAX_UPLOAD_FILES));
          if (files === null) {
            throw new UploadError(0, "image type not convertible");
          }
          paths = await uploadImages(files, life.signal);
        } catch (err: unknown) {
          if (life.signal.aborted) {
            return;
          }
          ctx.toast(
            err instanceof UploadError && err.status === 413 ? TOAST_TOO_LARGE : TOAST_FAILED,
          );
          return;
        }
        if (life.signal.aborted) {
          return;
        }
        if (ctx.session.id !== session) {
          ctx.toast(TOAST_TAB_CHANGED);
          return;
        }
        // Typed, not bracketed, and no Enter: the user finishes the prompt around it.
        ctx.send(new TextEncoder().encode(`${paths.join(" ")} `));
        if (images.length > MAX_UPLOAD_FILES) {
          ctx.toast(skippedNotice(images.length - MAX_UPLOAD_FILES));
        }
      }

      // Capture phase on the document, so a handled image never reaches the textarea's own
      // text-only paste listener; the body branch is the nothing-focused state.
      doc.addEventListener(
        "paste",
        (ev: ClipboardEvent) => {
          const target = ev.target;
          const ours =
            (target instanceof Node && surface.contains(target)) ||
            (target === doc.body && selectedPane());
          if (!ours) {
            return;
          }
          const images = clipboardImages(ev.clipboardData);
          if (images.length === 0) {
            return;
          }
          ev.preventDefault();
          ev.stopPropagation();
          void insertImages(images);
        },
        { capture: true, signal: life.signal },
      );

      async function pasteText(clip: Clipboard): Promise<void> {
        try {
          ctx.paste(await clip.readText());
        } catch {
          ctx.toast(TOAST_PASTE_BLOCKED);
        }
      }

      async function pasteFromClipboard(): Promise<void> {
        // Absent outside a secure context, whatever the DOM lib declares.
        const clip = (doc.defaultView?.navigator as { readonly clipboard?: Clipboard } | undefined)
          ?.clipboard;
        if (clip === undefined) {
          ctx.toast(TOAST_CLIPBOARD_UNAVAILABLE);
          return;
        }
        if (typeof clip.read !== "function") {
          await pasteText(clip);
          return;
        }
        let text: string;
        try {
          const items = await clip.read();
          const images: Blob[] = [];
          for (const item of items) {
            const type = item.types.find((t) => t.startsWith("image/"));
            if (type !== undefined) {
              images.push(await item.getType(type));
            }
          }
          if (images.length > 0) {
            await insertImages(images);
            return;
          }
          const textItem = items.find((item) => item.types.includes("text/plain"));
          text = textItem === undefined ? "" : await (await textItem.getType("text/plain")).text();
        } catch {
          // Every failure, a refused read() permission included, falls back to the UI's own
          // text-only read, so Ctrl+Shift+V without an image fails exactly as it always has.
          await pasteText(clip);
          return;
        }
        ctx.paste(text);
      }

      ctx.registerKeydown((ev) => {
        if (!isCtrlShiftV(ev)) {
          return false;
        }
        ev.preventDefault();
        void pasteFromClipboard();
        return true;
      });

      return { teardown: () => undefined };
    },
  };
}
