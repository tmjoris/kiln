// Kiln's front door on Cloudflare Workers (free plan).
//
// Clients call this Worker with an API key. It checks the key, keeps only Kiln's own routes,
// and forwards the request through a Cloudflare Tunnel to the Kiln gateway, adding a secret
// header that the gateway requires (KILN_EDGE_SECRET). Clients never see the origin or the
// secret, and the origin never sees the client's key.

export interface Env {
	ORIGIN_URL: string; // the tunnel's public URL, e.g. https://xyz.trycloudflare.com
	ORIGIN_SECRET: string; // secret: shared with the gateway
	API_KEY_HASHES: string; // secret: comma-separated SHA-256 hex digests of allowed client keys
}

const MAX_BODY_BYTES = 1 << 20;
const ROUTES = /^\/v1\/sandboxes(\/[A-Za-z0-9_-]+(\/(exec|files\/.*))?)?$/;

const enc = new TextEncoder();

async function sha256Hex(text: string): Promise<string> {
	const digest = await crypto.subtle.digest("SHA-256", enc.encode(text));
	return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// Constant-time comparison of two hex digests of the same length.
function sameDigest(a: string, b: string): boolean {
	const x = enc.encode(a);
	const y = enc.encode(b);
	return x.byteLength === y.byteLength && crypto.subtle.timingSafeEqual(x, y);
}

export async function keyAllowed(header: string | null, allowedHashes: string): Promise<boolean> {
	const key = header?.startsWith("Bearer ") ? header.slice(7).trim() : "";
	if (!key) return false;
	const digest = await sha256Hex(key);
	let ok = false;
	for (const h of allowedHashes.split(",").map((s) => s.trim().toLowerCase())) {
		if (h && sameDigest(digest, h)) ok = true; // no early exit, so timing doesn't reveal which key matched
	}
	return ok;
}

const json = (status: number, body: unknown) =>
	new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

export default {
	async fetch(request: Request, env: Env): Promise<Response> {
		const url = new URL(request.url);
		if (url.pathname === "/healthz") return json(200, { ok: true, edge: true });
		if (!ROUTES.test(url.pathname)) return json(404, { detail: "not found" });
		if (!["GET", "POST", "DELETE"].includes(request.method)) return json(405, { detail: "method not allowed" });
		if (Number(request.headers.get("content-length") ?? 0) > MAX_BODY_BYTES) {
			return json(413, { detail: "request body too large" });
		}

		const authStart = Date.now();
		if (!(await keyAllowed(request.headers.get("authorization"), env.API_KEY_HASHES ?? ""))) {
			return json(401, { detail: "missing or unknown API key" });
		}
		const authMs = Date.now() - authStart;

		const headers = new Headers(request.headers);
		headers.delete("authorization");
		headers.set("x-kiln-edge-secret", env.ORIGIN_SECRET);
		const originStart = Date.now();
		const upstream = await fetch(env.ORIGIN_URL.replace(/\/$/, "") + url.pathname + url.search, {
			method: request.method,
			headers,
			body: request.method === "GET" ? undefined : request.body,
		});
		const originMs = Date.now() - originStart;

		const response = new Response(upstream.body, upstream); // streams server-sent events through
		response.headers.append("server-timing", `edge-auth;dur=${authMs}, origin;dur=${originMs}`);
		return response;
	},
} satisfies ExportedHandler<Env>;
