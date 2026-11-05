import { createServer } from "node:http";
import { Entry } from "@napi-rs/keyring";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { UnauthorizedError, type OAuthClientProvider } from "@modelcontextprotocol/sdk/client/auth.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import type { OAuthClientInformationMixed, OAuthClientMetadata, OAuthTokens } from "@modelcontextprotocol/sdk/shared/auth.js";

// Atlassian's remote MCP, signed in with this app's own OAuth client (dynamic registration),
// kept in the macOS Keychain apart from pi's and Claude Code's.

const SERVER_URL = new URL("https://mcp.atlassian.com/v2/mcp");
const CALLBACK_PORT = 3127;
const REDIRECT_URL = `http://localhost:${CALLBACK_PORT}/callback`;
const KEYCHAIN_SERVICE = "foreman";

type Stored = { client?: OAuthClientInformationMixed; tokens?: OAuthTokens; verifier?: string };

const entry = new Entry(KEYCHAIN_SERVICE, "atlassian-mcp");
let cache: Stored | undefined;
const load = (): Stored => (cache ??= JSON.parse(entry.getPassword() ?? "{}") as Stored);
const save = (patch: Partial<Stored>) => {
  cache = { ...load(), ...patch };
  entry.setPassword(JSON.stringify(cache));
};

export function signOut(): void {
  cache = {};
  try {
    entry.deletePassword();
  } catch {}
}

class Provider implements OAuthClientProvider {
  constructor(private onAuthorizeUrl: (url: URL) => void) {}
  get redirectUrl() {
    return REDIRECT_URL;
  }
  get clientMetadata(): OAuthClientMetadata {
    return {
      client_name: "foreman",
      redirect_uris: [REDIRECT_URL],
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      token_endpoint_auth_method: "none",
    };
  }
  clientInformation() {
    return load().client;
  }
  saveClientInformation(client: OAuthClientInformationMixed) {
    save({ client });
  }
  tokens() {
    return load().tokens;
  }
  saveTokens(tokens: OAuthTokens) {
    save({ tokens });
  }
  redirectToAuthorization(url: URL) {
    this.onAuthorizeUrl(url);
  }
  saveCodeVerifier(verifier: string) {
    save({ verifier });
  }
  codeVerifier() {
    const v = load().verifier;
    if (!v) throw new Error("missing PKCE verifier");
    return v;
  }
  invalidateCredentials(scope: "all" | "client" | "tokens" | "verifier") {
    const s = load();
    if (scope === "all") return signOut();
    if (scope === "client") delete s.client;
    if (scope === "tokens") delete s.tokens;
    if (scope === "verifier") delete s.verifier;
    save(s);
  }
}

function waitForCode(): { code: Promise<string>; close: () => void } {
  let close = () => {};
  const code = new Promise<string>((resolve, reject) => {
    const server = createServer((req, res) => {
      const url = new URL(req.url ?? "/", REDIRECT_URL);
      if (url.pathname !== "/callback") return void res.writeHead(404).end();
      const code = url.searchParams.get("code");
      const error = url.searchParams.get("error_description") ?? url.searchParams.get("error");
      res.writeHead(200, { "content-type": "text/html" }).end(
        `<p style="font-family:system-ui">${code ? "foreman is signed in. You can close this tab." : `Sign-in failed: ${error}`}</p>`,
      );
      server.close();
      code ? resolve(code) : reject(new Error(`Atlassian sign-in failed: ${error}`));
    });
    server.on("error", reject);
    server.listen(CALLBACK_PORT, "127.0.0.1");
    close = () => server.close();
  });
  return { code, close };
}

export interface Mcp {
  call(tool: string, args: Record<string, unknown>): Promise<any>;
  close(): Promise<void>;
}

/** Connects, running the browser sign-in first when there are no valid tokens. */
export async function connect(log: (line: string) => void = (l) => console.error(l)): Promise<Mcp> {
  let authorizeUrl: URL | undefined;
  const provider = new Provider((url) => (authorizeUrl = url));
  const client = new Client({ name: "foreman", version: "0.1.0" });
  let transport = new StreamableHTTPClientTransport(SERVER_URL, { authProvider: provider });
  try {
    await client.connect(transport);
  } catch (e) {
    if (!(e instanceof UnauthorizedError) || !authorizeUrl) throw e;
    const { code, close } = waitForCode();
    log(`Opening the browser to sign in to Atlassian…\n${authorizeUrl}`);
    Bun.spawn(["open", authorizeUrl.toString()]);
    const timeout = setTimeout(close, 5 * 60_000);
    try {
      await transport.finishAuth(await code);
    } finally {
      clearTimeout(timeout);
    }
    transport = new StreamableHTTPClientTransport(SERVER_URL, { authProvider: provider });
    await client.connect(transport);
  }

  return {
    async call(tool, args) {
      const result = (await client.callTool({ name: tool, arguments: args })) as { content?: { type: string; text?: string }[]; isError?: boolean };
      const text = (result.content ?? [])
        .filter((c) => c.type === "text")
        .map((c) => c.text)
        .join("");
      if (result.isError) throw new Error(`${tool}: ${text.slice(0, 500)}`);
      const parsed = JSON.parse(text);
      return parsed.data ?? parsed;
    },
    close: () => client.close(),
  };
}
