# Sign in with ChatGPT: frontend handoff (karvon-fe)

The backend is done and tested. This document covers everything karvon-fe needs. For
the background, read `karvon-be/README.md` → "The AI generator".

## What it does for the user

Today the ChatGPT generator works in two ways. By default (manual) the operator copies
the prompt into ChatGPT and pastes the reply back. If the backend has an OpenAI API
key, it calls the API itself and each call is billed per token.

The new third option: the operator clicks **Connect ChatGPT**, signs in on OpenAI, and
generations then run on their **ChatGPT Plus/Pro plan**. There is no copy-paste and
no API bill. OpenAI enforces a weekly cap for each app, and the user sets it in their
ChatGPT settings.

**Status:** this stays off until OpenAI issues Karvon a client id (applied for through
OpenAI's interest form). While it is off, `GET /ai/provider` returns `chatgpt: null`.
The UI must hide every part of this feature in that case. Build it now against the MSW
mocks so it is ready when the id arrives.

---

## API contract

Regenerate the types first, with karvon-be running locally:
`pnpm gen:api` → `src/types/api.gen.ts`. Then re-export the new types from
`src/types/models.ts`: `ChatGPTConnection`, `ChatGPTConnectionStatus` and
`ChatGPTConnectStart`.

### `GET /ai/provider`: one new field

```jsonc
{
  "provider": "chatgpt_plan",        // new enum value; others: manual_chatgpt, openai_api
  "mode": "api",                     // "manual" | "api", as before
  "model": "gpt-5.6-terra",
  "label": "ChatGPT",
  "chatgpt": {                       // NEW. null = feature not configured → hide everything
    "status": "connected",           // "connected" | "needs_reconnect" | "disconnected"
    "email": "op@example.com",       // nullable
    "connected_at": "2026-10-07T…Z", // nullable
    "last_error": null               // nullable; set when status = needs_reconnect
  }
}
```

The backend picks the provider for each request. While `chatgpt.status = connected`,
`provider` is `chatgpt_plan` and `mode` is `api`. When the account is disconnected or
needs a reconnect, the response falls back to `openai_api` (if a key is set) or
`manual_chatgpt`. The existing `isManual` logic in `AIGeneratorPage.vue` keeps working
unchanged.

`GET /integrations` embeds the same object as `ai.chatgpt`.

### `POST /ai/chatgpt/connect` (new)

There is no request body. The response is `200 { "authorize_url": "https://auth.openai.com/…" }`.
Send the browser there with a full-page navigation: `window.location.assign(url)`.
Don't use a popup or an iframe, because OpenAI's page refuses to be framed and the
callback ends in a redirect. The URL is valid for 10 minutes and works once.
The endpoint returns `409 conflict` when the feature isn't configured.

### `GET /ai/chatgpt/callback`: backend only, do not call it

OpenAI sends the browser to the backend. The backend stores the tokens and then
redirects the browser to `KARVON_CHATGPT_RETURN_URL` with one of these query strings
added:

| Query | Meaning | Message to show |
| --- | --- | --- |
| `?chatgpt=connected` | Success | "ChatGPT connected. Generations now run on your ChatGPT plan." |
| `?chatgpt=error&reason=access_denied` | User clicked Cancel | "ChatGPT sign-in was cancelled." |
| `…&reason=expired` | Link older than 10 min, or reused | "That sign-in link expired. Try connecting again." |
| `…&reason=no_plan_access` | Not Plus/Pro, or sharing declined | "This ChatGPT account didn't allow plan usage. A Plus or Pro plan is required, and sharing must be allowed on OpenAI's consent screen." |
| `…&reason=failed` | Anything else | "ChatGPT sign-in failed. Try again." |
| `…&reason=not_configured` | Feature off | "Sign in with ChatGPT isn't configured on the server." |

Treat any unknown `reason` as `failed`.

**Ask the backend to set** `KARVON_CHATGPT_RETURN_URL=<fe-origin>/settings/integrations#campaigns`.
The query string lands before the hash, so the result is
`/settings/integrations?chatgpt=connected#campaigns`. If it is left unset, the browser
returns to the bare CORS origin (`/`). A small global fallback is therefore useful;
see item 4.

### `DELETE /ai/chatgpt` (new)

Returns `204`. Karvon forgets the tokens. To revoke access on OpenAI's side as well,
the user removes Karvon under ChatGPT → Settings → Connected apps. Say so in the
confirm dialog.

### `POST /ai/generations`: one new field and two new failures

- **New body field `manual: boolean`** (default `false`). It forces the copy-paste flow
  even while a ChatGPT account is connected. The generation returns
  `provider: "manual_chatgpt"` and `status: "awaiting_paste"`, the same as today's
  manual flow.
- **`429`, `error.code = "ai_plan_limit"`.** The plan's weekly cap for Karvon is used
  up. The backend never bills the API key instead, by design. Show:
  "Your ChatGPT plan has no usage left for Karvon this week. Raise the cap in ChatGPT
  settings, or continue with copy-paste." Offer a **Continue with copy-paste** button
  that resubmits the same brief with `manual: true`.
- **`502`, `error.code = "provider_auth"`, while the provider was `chatgpt_plan`.**
  The sign-in was revoked or has expired. The backend has already flipped the status
  to `needs_reconnect`. Invalidate `aiKeys.provider()` and `integrationKeys.all()`
  and show "ChatGPT needs reconnecting" with a **Reconnect** action (same as Connect).

---

## Work items

### 1. API + composables (`src/modules/campaigns/`)

- `api.ts`, in `aiApi`: add `connectChatGPT: () => apiPost<ChatGPTConnectStart>('/ai/chatgpt/connect')`
  and `disconnectChatGPT: () => apiDelete<void>('/ai/chatgpt')`. That uses the existing DELETE
  helper in `src/lib/api.ts`.
- `composables/useAI.ts`:
  - `useConnectChatGPT()`: a mutation whose `onSuccess` calls
    `window.location.assign(data.authorize_url)`. Keep the button in a loading state,
    because the page is about to leave.
  - `useDisconnectChatGPT()`: a mutation that invalidates `aiKeys.all()` and
    `integrationKeys.all()`, then toasts.
  - `useCreateGeneration()`: special-case `ApiError.code === 'ai_plan_limit'` and
    `'provider_auth'` in `onError` (see above). It's cleaner to surface these to the
    page than to only toast, because the page needs to render the copy-paste and
    Reconnect actions.
  - `CreateGenerationBody` gains `manual?: boolean` once the types are regenerated.
  - Optional: drop `useAIProvider`'s 5-minute `staleTime` to something short, or
    invalidate it on window focus, so a revoked connection shows up promptly.

### 2. Settings → Integrations, the "Copy generation" card

File: `src/modules/settings/pages/IntegrationsPage.vue`, the `<Card>` titled "Copy
generation" (around line 218). When `integrations.ai.chatgpt` is not null, add a
**ChatGPT account** block:

| `chatgpt.status` | Show |
| --- | --- |
| `disconnected` | "Run generations on your ChatGPT Plus or Pro plan instead of copy-paste or an API key." Button **Connect ChatGPT**. |
| `connected` | Green badge "Connected", `email`, "since {connected_at}". Short note: "Uses your ChatGPT plan's weekly allowance for Karvon. Set the cap in ChatGPT settings." Button **Disconnect** (with confirm). |
| `needs_reconnect` | Amber badge "Needs reconnecting", `last_error` in muted small text. Button **Reconnect**. |

Also rewrite the stale help text under `mode === 'manual'`. It currently says "A
ChatGPT subscription includes no API access"; that stopped being true when OpenAI
launched plan sharing. When `chatgpt` is not null, point to the Connect button
instead. When it is null, keep the API-key hint but drop the "no API access" claim.

### 3. Handle the return from OpenAI

On `IntegrationsPage.vue` mount, read `route.query.chatgpt` and `route.query.reason`.
Toast according to the table above, invalidate `aiKeys.all()` and
`integrationKeys.all()`, then `router.replace({ query: {}, hash: route.hash })` so a
refresh doesn't toast again. Scroll to `#campaigns`, which the hash already does.

### 4. Global fallback (small)

If the return URL is left at its default, the browser lands on `/?chatgpt=…`. Add the
same query handling to the root route or the app shell: toast, then redirect to
`/settings/integrations#campaigns`. About ten lines in `src/app` or
`src/router/index.ts`.

### 5. AI generator page

File: `src/modules/campaigns/pages/AIGeneratorPage.vue`.

- Header badge: when `provider.provider === 'chatgpt_plan'`, show "ChatGPT plan ·
  {email}" instead of the plain label.
- The "Manual mode" alert (around line 114) repeats the outdated "no API access" text.
  When `provider.chatgpt?.status === 'disconnected'`, replace it with "Connect your
  ChatGPT account in Settings → Integrations to skip copy-paste." and link to it.
  When the status is `needs_reconnect`, show a warning alert with a Reconnect button.
- On `ai_plan_limit`, show an inline alert above the brief with **Continue with
  copy-paste**, which resubmits the brief with `manual: true`. The existing manual UI
  (copy prompt, paste reply) then takes over for that generation.

### 6. MSW mocks: `tests/mocks/handlers/campaigns.ts`

- `GET /ai/provider` (around line 1251): add `chatgpt`, driven by mock state so stories
  and tests can switch between `null`, disconnected, connected and needs_reconnect.
- `POST /ai/chatgpt/connect`: return
  `{ authorize_url: '/settings/integrations?chatgpt=connected#campaigns' }`, so in mock
  mode the "redirect" loops straight back as a success. Flip the mock state to
  connected.
- `DELETE /ai/chatgpt`: return 204 and flip the state to disconnected.
- `POST /ai/generations`: let a mock flag return
  `429 { error: { code: 'ai_plan_limit', message: '…' } }`.

### 7. Tests (vitest)

- The Integrations card renders the right block for each of the 4 `chatgpt` states,
  including `null`, which renders nothing.
- The return-query handler toasts the right message for each `reason` and clears the
  query.
- The generator shows the copy-paste fallback on `ai_plan_limit`, and the resubmit
  sends `manual: true`.
- The definition of done is `pnpm typecheck`, `pnpm lint` and `pnpm test` all green.

---

## Don'ts

- Don't open the authorize URL in a popup or an iframe, and don't build or modify it
  on the client. Always use the URL the backend returns: it carries the PKCE challenge
  and the state.
- Don't call `/ai/chatgpt/callback` from the app.
- Don't store anything about the tokens client-side. The frontend never sees them.
- Don't automatically retry a generation on `ai_plan_limit`. Retrying won't help until
  the cap resets.

## Local testing against the real backend

The backend only enables this feature with `KARVON_CHATGPT_CLIENT_ID` and a public base
URL. Until OpenAI issues the id, use the MSW mocks. Once it arrives, the backend sets:

```
KARVON_CHATGPT_CLIENT_ID=<from OpenAI>
KARVON_PUBLIC_BASE_URL=<backend origin, must match the redirect URI registered with OpenAI>
KARVON_CHATGPT_RETURN_URL=<fe-origin>/settings/integrations#campaigns
```
