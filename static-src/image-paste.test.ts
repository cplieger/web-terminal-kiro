import {
  createTerminal,
  type TerminalContext,
  type TerminalHandle,
} from "@cplieger/web-terminal-ui";
import { clipboard } from "@cplieger/web-terminal-ui/features/clipboard";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  TOAST_FAILED,
  TOAST_TAB_CHANGED,
  TOAST_TOO_LARGE,
  downscaleImage,
  imagePaste,
  pastedImageName,
  prepareForUpload,
} from "./image-paste.js";

const STAMP = new Date("2026-08-15T08:42:11.123Z");

async function png(width: number, height: number): Promise<Blob> {
  const canvas = new OffscreenCanvas(width, height);
  const ctx = canvas.getContext("2d");
  if (ctx === null) {
    throw new Error("no 2d context");
  }
  ctx.fillStyle = "#c099ff";
  ctx.fillRect(0, 0, width, height);
  return canvas.convertToBlob({ type: "image/png" });
}

async function size(blob: Blob): Promise<[number, number]> {
  const bmp = await createImageBitmap(blob);
  const dims: [number, number] = [bmp.width, bmp.height];
  bmp.close();
  return dims;
}

describe("pastedImageName", () => {
  it("is marotte's UTC second stamp with an extension for the type", () => {
    expect(pastedImageName("image/png", STAMP)).toBe("pasted-2026-08-15T08-42-11.png");
    expect(pastedImageName("image/jpeg", STAMP)).toBe("pasted-2026-08-15T08-42-11.jpg");
    expect(pastedImageName("image/webp", STAMP)).toBe("pasted-2026-08-15T08-42-11.webp");
    expect(pastedImageName("image/gif", STAMP)).toBe("pasted-2026-08-15T08-42-11.png");
  });

  it("suffixes the second and later images of one paste", () => {
    expect(pastedImageName("image/png", STAMP, 1)).toBe("pasted-2026-08-15T08-42-11-2.png");
    expect(pastedImageName("image/png", STAMP, 24)).toBe("pasted-2026-08-15T08-42-11-25.png");
  });
});

describe("downscaleImage", () => {
  it("scales the long edge to 2000 px and keeps the type", async () => {
    const out = await downscaleImage(await png(4000, 1000));
    expect(out.type).toBe("image/png");
    expect(await size(out)).toEqual([2000, 500]);
  });

  it("returns an image already within bounds untouched", async () => {
    const small = await png(100, 100);
    expect(await downscaleImage(small)).toBe(small);
  });

  it("re-encodes a type the server does not accept to PNG", async () => {
    const bmp = await png(10, 10);
    const gif = new Blob([await bmp.arrayBuffer()], { type: "image/gif" });
    const out = await downscaleImage(gif);
    expect(out.type).toBe("image/png");
    expect(await size(out)).toEqual([10, 10]);
  });

  it("returns the original when it cannot be decoded", async () => {
    const corrupt = new Blob(["not an image"], { type: "image/png" });
    expect(await downscaleImage(corrupt)).toBe(corrupt);
  });

  it("names prepared files from the bytes' actual type", async () => {
    const files = await prepareForUpload(
      [await png(10, 10), new Blob(["x"], { type: "image/jpeg" })],
      STAMP,
    );
    expect(files?.map((f) => [f.name, f.type])).toEqual([
      ["pasted-2026-08-15T08-42-11.png", "image/png"],
      ["pasted-2026-08-15T08-42-11-2.jpg", "image/jpeg"],
    ]);
  });

  it("refuses a batch holding an image it could not convert to a kept type", async () => {
    const tiff = new Blob(["II*\u0000not decodable"], { type: "image/tiff" });
    expect(await prepareForUpload([await png(10, 10), tiff], STAMP)).toBeNull();
  });
});

// A hand-built context: the feature reaches the terminal only through these members.
interface FakeTerminal {
  ctx: TerminalContext;
  surface: HTMLElement;
  textarea: HTMLTextAreaElement;
  send: ReturnType<typeof vi.fn>;
  paste: ReturnType<typeof vi.fn>;
  toast: ReturnType<typeof vi.fn>;
  session: { id: string | null };
  selected: { side: "left" | "right" };
  keydown: (ev: KeyboardEvent) => boolean;
  teardown: () => void;
}

function mountFake(): FakeTerminal {
  const paneRoot = document.createElement("div");
  const surface = document.createElement("div");
  const textarea = document.createElement("textarea");
  surface.append(textarea);
  paneRoot.append(surface);
  document.body.append(paneRoot);
  const send = vi.fn();
  const paste = vi.fn();
  const toast = vi.fn();
  const session = { id: "s1" as string | null };
  const selected = { side: "left" as "left" | "right" };
  const keydowns: ((ev: KeyboardEvent) => boolean)[] = [];
  const releases: (() => void)[] = [];
  const ctx = {
    surface: () => surface,
    send,
    paste,
    toast,
    session,
    shell: {
      root: paneRoot,
      panes: () => [{ side: "left", root: paneRoot }],
      selected: () => selected.side,
    },
    registerKeydown: (fn: (ev: KeyboardEvent) => boolean) => {
      keydowns.push(fn);
      return () => undefined;
    },
    defer: (fn: () => void) => {
      releases.push(fn);
    },
  } as unknown as TerminalContext;
  const instance = imagePaste().setup(ctx);
  if (instance instanceof Promise) {
    throw new Error("imagePaste().setup must be synchronous");
  }
  return {
    ctx,
    surface,
    textarea,
    send,
    paste,
    toast,
    session,
    selected,
    keydown: (ev) => keydowns.some((fn) => fn(ev)),
    teardown: () => {
      instance.teardown();
      for (const r of releases.reverse()) {
        r();
      }
    },
  };
}

function pasteEvent(items: { files?: File[]; text?: string }): ClipboardEvent {
  const dt = new DataTransfer();
  for (const f of items.files ?? []) {
    dt.items.add(f);
  }
  if (items.text !== undefined) {
    dt.setData("text/plain", items.text);
  }
  return new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true });
}

function okResponse(paths: string[]): Response {
  return new Response(JSON.stringify({ uploaded: paths }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

async function imageFile(): Promise<File> {
  return new File([await png(8, 8)], "image.png", { type: "image/png" });
}

const PATH = "/uploads/pasted-2026-08-15T08-42-11.png";

function sentText(send: ReturnType<typeof vi.fn>): string {
  const bytes = send.mock.calls[0]?.[0] as Uint8Array;
  return new TextDecoder().decode(bytes);
}

describe("imagePaste: the paste event", () => {
  let fake: FakeTerminal;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(() => Promise.resolve(okResponse([PATH])));
    vi.stubGlobal("fetch", fetchMock);
    fake = mountFake();
  });

  afterEach(() => {
    fake.teardown();
    document.body.replaceChildren();
  });

  it("uploads an image paste and types its path plus a space, without Enter", async () => {
    const reachedTextarea = vi.fn();
    fake.textarea.addEventListener("paste", reachedTextarea);
    const ev = pasteEvent({ files: [await imageFile()] });

    fake.textarea.dispatchEvent(ev);

    expect(ev.defaultPrevented).toBe(true);
    expect(reachedTextarea).not.toHaveBeenCalled();
    await vi.waitFor(() => {
      expect(fake.send).toHaveBeenCalledTimes(1);
    });
    expect(sentText(fake.send)).toBe(`${PATH} `);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/uploads");
    expect(init.method).toBe("POST");
    const sent = (init.body as FormData).getAll("files") as File[];
    expect(sent).toHaveLength(1);
    expect(sent[0]?.name).toMatch(/^pasted-\d{4}-\d\d-\d\dT\d\d-\d\d-\d\d\.png$/);
    expect(fake.paste).not.toHaveBeenCalled();
  });

  it("leaves a text-only paste entirely to the UI library", () => {
    const reachedTextarea = vi.fn();
    fake.textarea.addEventListener("paste", reachedTextarea);
    const ev = pasteEvent({ text: "hello\nworld" });

    fake.textarea.dispatchEvent(ev);

    expect(ev.defaultPrevented).toBe(false);
    expect(reachedTextarea).toHaveBeenCalledTimes(1);
    expect(fetchMock).not.toHaveBeenCalled();
    expect(fake.send).not.toHaveBeenCalled();
    expect(fake.paste).not.toHaveBeenCalled();
  });

  it("takes the image when the paste carries both an image and text", async () => {
    const ev = pasteEvent({ files: [await imageFile()], text: "caption" });
    fake.textarea.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
    await vi.waitFor(() => {
      expect(fake.send).toHaveBeenCalledTimes(1);
    });
    expect(fake.paste).not.toHaveBeenCalled();
  });

  it("ignores an image pasted outside this terminal", async () => {
    const elsewhere = document.createElement("input");
    document.body.append(elsewhere);
    const ev = pasteEvent({ files: [await imageFile()] });
    elsewhere.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("handles a body-target paste only in the selected pane", async () => {
    fake.selected.side = "right";
    const notOurs = pasteEvent({ files: [await imageFile()] });
    document.body.dispatchEvent(notOurs);
    expect(notOurs.defaultPrevented).toBe(false);

    fake.selected.side = "left";
    const ours = pasteEvent({ files: [await imageFile()] });
    document.body.dispatchEvent(ours);
    expect(ours.defaultPrevented).toBe(true);
    await vi.waitFor(() => {
      expect(fake.send).toHaveBeenCalledTimes(1);
    });
  });

  it.each([
    ["a 413", () => Promise.resolve(new Response("{}", { status: 413 })), TOAST_TOO_LARGE],
    ["a 500", () => Promise.resolve(new Response("{}", { status: 500 })), TOAST_FAILED],
    ["a network failure", () => Promise.reject(new TypeError("Failed to fetch")), TOAST_FAILED],
    [
      "a response carrying a control sequence",
      () => Promise.resolve(okResponse(["\u001b[2J"])),
      TOAST_FAILED,
    ],
    ["a response that is not an object", () => Promise.resolve(new Response("null")), TOAST_FAILED],
    [
      "a response listing more paths than files sent",
      () => Promise.resolve(okResponse([PATH, PATH])),
      TOAST_FAILED,
    ],
    [
      "a response naming a path outside /uploads",
      () => Promise.resolve(okResponse(["/etc/passwd"])),
      TOAST_FAILED,
    ],
  ])("toasts and types nothing on %s", async (_desc, respond, toast) => {
    fetchMock.mockImplementation(respond);
    fake.textarea.dispatchEvent(pasteEvent({ files: [await imageFile()] }));
    await vi.waitFor(() => {
      expect(fake.toast).toHaveBeenCalledExactlyOnceWith(toast);
    });
    expect(fake.send).not.toHaveBeenCalled();
  });

  it("refuses an image it cannot convert, without uploading it", async () => {
    const tiff = new File(["II*\u0000not decodable"], "scan.tiff", { type: "image/tiff" });
    fake.textarea.dispatchEvent(pasteEvent({ files: [tiff] }));
    await vi.waitFor(() => {
      expect(fake.toast).toHaveBeenCalledExactlyOnceWith(TOAST_FAILED);
    });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(fake.send).not.toHaveBeenCalled();
  });

  it("uploads the first 25 of 26 images and says one was skipped", async () => {
    const paths = Array.from(
      { length: 25 },
      (_, i) => `/uploads/pasted-2026-08-15T08-42-11${i === 0 ? "" : `-${String(i + 1)}`}.png`,
    );
    fetchMock.mockImplementation(() => Promise.resolve(okResponse(paths)));
    const files = await Promise.all(Array.from({ length: 26 }, () => imageFile()));

    fake.textarea.dispatchEvent(pasteEvent({ files }));

    await vi.waitFor(() => {
      expect(fake.send).toHaveBeenCalledTimes(1);
    });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect((init.body as FormData).getAll("files")).toHaveLength(25);
    expect(sentText(fake.send)).toBe(`${paths.join(" ")} `);
    expect(fake.toast).toHaveBeenCalledExactlyOnceWith("Skipped 1 image: over 25 at once");
  });

  it("says nothing was skipped for exactly 25 images", async () => {
    const paths = Array.from(
      { length: 25 },
      (_, i) => `/uploads/pasted-2026-08-15T08-42-11${i === 0 ? "" : `-${String(i + 1)}`}.png`,
    );
    fetchMock.mockImplementation(() => Promise.resolve(okResponse(paths)));
    const files = await Promise.all(Array.from({ length: 25 }, () => imageFile()));

    fake.textarea.dispatchEvent(pasteEvent({ files }));

    await vi.waitFor(() => {
      expect(fake.send).toHaveBeenCalledTimes(1);
    });
    expect(fake.toast).not.toHaveBeenCalled();
  });

  it("does not type into a tab switched to while the upload ran", async () => {
    let release: (r: Response) => void = () => undefined;
    fetchMock.mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          release = resolve;
        }),
    );
    fake.textarea.dispatchEvent(pasteEvent({ files: [await imageFile()] }));
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });
    fake.session.id = "s2";
    release(okResponse([PATH]));
    await vi.waitFor(() => {
      expect(fake.toast).toHaveBeenCalledExactlyOnceWith(TOAST_TAB_CHANGED);
    });
    expect(fake.send).not.toHaveBeenCalled();
  });

  it("drops an upload in flight when the feature is released", async () => {
    let release: (r: Response) => void = () => undefined;
    fetchMock.mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          release = resolve;
        }),
    );
    fake.textarea.dispatchEvent(pasteEvent({ files: [await imageFile()] }));
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });
    fake.teardown();
    release(okResponse([PATH]));
    await new Promise((r) => setTimeout(r, 20));
    expect(fake.send).not.toHaveBeenCalled();
    expect(fake.toast).not.toHaveBeenCalled();
    fake = mountFake(); // afterEach tears down a live one
  });

  it("aborts the request, without a failure toast, when the feature is released", async () => {
    let signal: AbortSignal | undefined;
    fetchMock.mockImplementation(
      (_url: string, init: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          signal = init.signal ?? undefined;
          signal?.addEventListener("abort", () => {
            reject(signal?.reason);
          });
        }),
    );
    fake.textarea.dispatchEvent(pasteEvent({ files: [await imageFile()] }));
    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });
    fake.teardown();
    await new Promise((r) => setTimeout(r, 20));
    expect(signal?.aborted).toBe(true);
    expect(fake.toast).not.toHaveBeenCalled();
    expect(fake.send).not.toHaveBeenCalled();
    fake = mountFake();
  });

  it("ignores a paste event that carries no clipboard data", () => {
    const ev = new ClipboardEvent("paste", { bubbles: true, cancelable: true });
    fake.textarea.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("stops listening once the feature is released", async () => {
    fake.teardown();
    const ev = pasteEvent({ files: [await imageFile()] });
    fake.textarea.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(false);
    fake = mountFake(); // afterEach tears down a live one
  });
});

function ctrlShiftV(init: KeyboardEventInit = {}): KeyboardEvent {
  return new KeyboardEvent("keydown", {
    code: "KeyV",
    key: "V",
    ctrlKey: true,
    shiftKey: true,
    bubbles: true,
    cancelable: true,
    ...init,
  });
}

function stubClipboard(clip: Partial<Clipboard> | undefined): void {
  Object.defineProperty(navigator, "clipboard", { value: clip, configurable: true });
}

describe("imagePaste: Ctrl+Shift+V", () => {
  let fake: FakeTerminal;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(() => Promise.resolve(okResponse([PATH])));
    vi.stubGlobal("fetch", fetchMock);
    fake = mountFake();
  });

  afterEach(() => {
    fake.teardown();
    Reflect.deleteProperty(navigator, "clipboard");
    document.body.replaceChildren();
  });

  it("uploads a clipboard image and claims the key", async () => {
    const readText = vi.fn();
    stubClipboard({
      read: () => Promise.resolve([new ClipboardItem({ "image/png": png(8, 8) })]),
      readText,
    });
    const ev = ctrlShiftV();

    expect(fake.keydown(ev)).toBe(true);
    expect(ev.defaultPrevented).toBe(true);
    await vi.waitFor(() => {
      expect(fake.send).toHaveBeenCalledTimes(1);
    });
    expect(sentText(fake.send)).toBe(`${PATH} `);
    expect(readText).not.toHaveBeenCalled();
    expect(fake.paste).not.toHaveBeenCalled();
  });

  it("pastes clipboard text through the same funnel as today", async () => {
    stubClipboard({
      read: () =>
        Promise.resolve([
          new ClipboardItem({ "text/plain": new Blob(["hello\nworld"], { type: "text/plain" }) }),
        ]),
    });
    fake.keydown(ctrlShiftV());
    await vi.waitFor(() => {
      expect(fake.paste).toHaveBeenCalledExactlyOnceWith("hello\nworld");
    });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(fake.send).not.toHaveBeenCalled();
  });

  it("falls back to readText where read is absent", async () => {
    stubClipboard({ readText: () => Promise.resolve("plain") });
    fake.keydown(ctrlShiftV());
    await vi.waitFor(() => {
      expect(fake.paste).toHaveBeenCalledExactlyOnceWith("plain");
    });
  });

  it("falls back to readText when read fails for a reason other than permission", async () => {
    stubClipboard({
      read: () => Promise.reject(new DOMException("unsupported", "DataError")),
      readText: () => Promise.resolve("plain"),
    });
    fake.keydown(ctrlShiftV());
    await vi.waitFor(() => {
      expect(fake.paste).toHaveBeenCalledExactlyOnceWith("plain");
    });
  });

  it("falls back to readText when the read permission is refused", async () => {
    stubClipboard({
      read: () => Promise.reject(new DOMException("denied", "NotAllowedError")),
      readText: () => Promise.resolve("plain"),
    });
    fake.keydown(ctrlShiftV());
    await vi.waitFor(() => {
      expect(fake.paste).toHaveBeenCalledExactlyOnceWith("plain");
    });
    expect(fake.toast).not.toHaveBeenCalled();
  });

  it("toasts once when both read and readText are refused", async () => {
    stubClipboard({
      read: () => Promise.reject(new DOMException("denied", "NotAllowedError")),
      readText: () => Promise.reject(new DOMException("denied", "NotAllowedError")),
    });
    fake.keydown(ctrlShiftV());
    await vi.waitFor(() => {
      expect(fake.toast).toHaveBeenCalledExactlyOnceWith("Paste blocked");
    });
    expect(fake.paste).not.toHaveBeenCalled();
  });

  it("toasts when readText is refused", async () => {
    stubClipboard({
      readText: () => Promise.reject(new DOMException("denied", "NotAllowedError")),
    });
    fake.keydown(ctrlShiftV());
    await vi.waitFor(() => {
      expect(fake.toast).toHaveBeenCalledExactlyOnceWith("Paste blocked");
    });
  });

  it("toasts outside a secure context", async () => {
    stubClipboard(undefined);
    fake.keydown(ctrlShiftV());
    await vi.waitFor(() => {
      expect(fake.toast).toHaveBeenCalledExactlyOnceWith("Clipboard unavailable");
    });
  });

  it.each([
    ["Ctrl+V", { shiftKey: false }],
    ["Ctrl+Alt+Shift+V", { altKey: true }],
    ["Ctrl+Meta+Shift+V", { metaKey: true }],
    ["Ctrl+Shift+C", { code: "KeyC", key: "C" }],
  ])("leaves %s to the rest of the chain", (_desc, init) => {
    const ev = ctrlShiftV(init);
    expect(fake.keydown(ev)).toBe(false);
    expect(ev.defaultPrevented).toBe(false);
  });
});

describe("imagePaste ahead of the UI clipboard feature", () => {
  let handle: TerminalHandle | undefined;

  afterEach(() => {
    handle?.destroy();
    handle = undefined;
    Reflect.deleteProperty(navigator, "clipboard");
    document.body.replaceChildren();
  });

  it("claims Ctrl+Shift+V before the clipboard feature's text-only read", async () => {
    const read = vi.fn(() => Promise.resolve([] as ClipboardItem[]));
    const readText = vi.fn(() => Promise.resolve(""));
    stubClipboard({ read, readText });
    const container = document.createElement("div");
    container.style.height = "300px";
    document.body.append(container);
    handle = createTerminal(container, {
      layout: "container",
      wsPath: "/__no_ws__",
      features: () => [imagePaste(), clipboard()],
    });
    const input = await vi.waitFor(() => {
      const el = container.querySelector<HTMLTextAreaElement>(".term-input");
      if (el === null) {
        throw new Error("terminal not mounted");
      }
      return el;
    });
    input.focus();

    input.dispatchEvent(ctrlShiftV());

    await vi.waitFor(() => {
      expect(read).toHaveBeenCalledTimes(1);
    });
    expect(readText).not.toHaveBeenCalled();
  });
});
