// Thin API client with typed decoder support. Subset of vibekit's
// api-client.ts — only what vibecli's tiny surface needs.

export type { Decoder } from "./validators.js";
import type { Decoder } from "./validators.js";

const JSON_HEADERS = { "Content-Type": "application/json" };
const API_TIMEOUT_MS = 10_000;

function withTimeout(signal: AbortSignal | undefined, ms: number): AbortSignal {
  return signal !== undefined
    ? AbortSignal.any([signal, AbortSignal.timeout(ms)])
    : AbortSignal.timeout(ms);
}

/** GET `path`, validate with `decoder`, return T or null. */
export async function apiGetTyped<T>(path: string, decoder: Decoder<T>, signal?: AbortSignal): Promise<T | null> {
  try {
    const r = await fetch(path, { signal: withTimeout(signal, API_TIMEOUT_MS) });
    if (!r.ok) { console.warn("api: non-ok GET", path, r.status); return null; }
    const parsed: unknown = await r.json();
    return decoder(parsed);
  } catch (e) {
    console.warn("api: GET failed", path, e);
    return null;
  }
}

/** POST `body` as JSON to `path`, validate with `decoder`, return T or null. */
export async function apiPostTyped<T>(path: string, body: unknown, decoder: Decoder<T>, signal?: AbortSignal): Promise<T | null> {
  try {
    const r = await fetch(path, {
      method: "POST",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
      signal: withTimeout(signal, API_TIMEOUT_MS),
    });
    if (!r.ok) { console.warn("api: non-ok POST", path, r.status); return null; }
    const parsed: unknown = await r.json();
    return decoder(parsed);
  } catch (e) {
    console.warn("api: POST failed", path, e);
    return null;
  }
}
