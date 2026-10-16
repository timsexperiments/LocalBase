import { describe, expect, test } from "bun:test";
import {
  discardLegacyFragmentCredential,
  type Conversation,
  type Mode,
} from "./client";
import {
  administrationSections,
  canOpenPanel,
  conversationNavigation,
  detailsUrl,
  navigationUrl,
  readDetails,
  readNavigation,
  resolveNavigation,
  startConversation,
  writeNavigation,
  type Navigation,
} from "./navigation";

const saved: Conversation = {
  id: "local-conversation",
  workspace: "lab",
  mode: "image",
  model: "image:flux/q4",
  title: "Private title",
  messages: [{ id: "message", role: "user", text: "Private prompt" }],
};
function fresh(mode: Mode, workspace: Conversation["workspace"]): Conversation {
  return {
    id: "fresh-local-id",
    mode,
    workspace,
    model: "",
    title: "New conversation",
    messages: [],
  };
}

describe("playground navigation boundary", () => {
  test("requires the matching permission for administrative panels", () => {
    expect(canOpenPanel("catalog", null)).toBe(false);
    expect(canOpenPanel("catalog", ["models:read"])).toBe(false);
    expect(canOpenPanel("catalog", ["models:manage"])).toBe(true);
    expect(canOpenPanel("admin", ["access:read"])).toBe(true);
    expect(canOpenPanel("admin", ["keys:read"])).toBe(true);
    expect(canOpenPanel("admin", ["access:manage", "keys:manage"])).toBe(false);
    expect(administrationSections(["keys:read"])).toEqual(["keys"]);
    expect(administrationSections(["access:read", "keys:read"])).toEqual([
      "people",
      "access",
      "keys",
    ]);
    expect(canOpenPanel("models", [])).toBe(true);
    expect(canOpenPanel(null, null)).toBe(true);
  });

  test("missing and invalid query fields fall back independently", () => {
    const defaults: Navigation = {
      view: "chat",
      mode: "llm",
      model: "",
      panel: null,
      conversation: "",
    };
    expect(readNavigation("")).toEqual(defaults);
    expect(
      readNavigation(
        "?view=other&mode=invalid&panel=bad&model=%00&conversation=has+spaces",
      ),
    ).toEqual(defaults);
    expect(readNavigation("?view=lab&mode=image&panel=invalid")).toEqual({
      ...defaults,
      view: "lab",
      mode: "image",
    });
    expect(readNavigation("?view=chat&mode=video").mode).toBe("llm");
    expect(readNavigation("?model=" + "x".repeat(513)).model).toBe("");
  });

  test("all modes and panels round trip with an encoded canonical model id", () => {
    for (const mode of [
      "llm",
      "image",
      "tts",
      "stt",
      "video",
      "embedding",
      "decision",
    ] as const) {
      for (const panel of [
        null,
        "models",
        "settings",
        "history",
        "generation",
        "catalog",
        "admin",
      ] as const) {
        const route = conversationNavigation(
          { ...saved, mode, model: "vendor/model:q4@v1" },
          panel,
        );
        const url = navigationUrl(
          "https://local.test/app?unrelated=kept",
          route,
        );
        expect(
          readNavigation(new URL(url, "https://local.test").search),
        ).toEqual(route);
      }
    }
  });

  test("resolves only local conversations and preserves their context and messages", () => {
    const resolved = resolveNavigation({
      navigation: readNavigation(
        "?view=chat&mode=llm&conversation=local-conversation&panel=history&model=image:new",
      ),
      conversations: [saved],
      fresh,
    });
    expect(resolved.conversation).toEqual({ ...saved, model: "image:new" });
    expect(resolved.navigation).toEqual(
      conversationNavigation(resolved.conversation, "history"),
    );
    expect(resolved.conversations).toHaveLength(1);
    expect(saved.model).toBe("image:flux/q4");
  });

  test("missing or absent conversation IDs create fresh state, never the first saved history item", () => {
    for (const query of ["", "&conversation=not-on-this-device"]) {
      const resolved = resolveNavigation({
        navigation: readNavigation("?view=lab&mode=video" + query),
        conversations: [saved],
        fresh,
      });
      expect(resolved.conversation).toEqual(fresh("video", "lab"));
      expect(resolved.navigation.conversation).toBe("fresh-local-id");
      expect(resolved.conversations[1]).toBe(saved);
    }
  });

  test("canonical navigation drops unknown query fields, credentials and content", () => {
    const url = navigationUrl(
      "https://local.test/app?keep=a&keep=b&key=secret&token=secret&authorization=secret&prompt=Private&history=Private#key=secret",
      conversationNavigation(saved, "generation"),
    );
    expect(url).not.toContain("secret");
    expect(url).not.toContain("Private");
    expect(url).not.toContain("message");
    expect(
      new URL(url, "https://local.test").searchParams.getAll("keep"),
    ).toEqual([]);
    expect([...new URL(url, "https://local.test").searchParams.keys()]).toEqual(
      ["view", "mode", "model", "panel", "conversation"],
    );
  });

  test("keeps catalog filters only while the catalog panel is open", () => {
    const href =
      "https://local.test/app?panel=catalog&q=coder&capability=tool-calling&capability=reasoning&sort=size&enabled=true&token=secret";
    const open = new URL(
      navigationUrl(href, conversationNavigation(saved, "catalog")),
      "https://local.test",
    ).searchParams;
    expect(open.get("q")).toBe("coder");
    expect(open.getAll("capability")).toEqual(["tool-calling", "reasoning"]);
    expect(open.get("sort")).toBe("size");
    expect(open.get("enabled")).toBe("true");
    expect(open.has("token")).toBe(false);
    const closed = navigationUrl(href, conversationNavigation(saved, null));
    expect(closed).not.toContain("coder");
    expect(closed).not.toContain("capability");
  });

  test("keeps an open model details view linkable with the catalog", () => {
    const href =
      "https://local.test/app?panel=catalog&q=coder&details=qwen2.5-coder-1.5b-instruct-q4_k_m";
    expect(readDetails(new URL(href).search)).toBe(
      "qwen2.5-coder-1.5b-instruct-q4_k_m",
    );
    expect(readDetails("?details=")).toBeNull();
    expect(readDetails("?details=..%2Fescape")).toBeNull();
    expect(readDetails("?panel=catalog")).toBeNull();
    const open = navigationUrl(href, conversationNavigation(saved, "catalog"));
    expect(new URL(open, href).searchParams.get("details")).toBe(
      "qwen2.5-coder-1.5b-instruct-q4_k_m",
    );
    expect(
      navigationUrl(href, conversationNavigation(saved, null)),
    ).not.toContain("details");
    expect(
      detailsUrl("https://local.test/app?panel=catalog", "whisper-base-q8_0"),
    ).toBe("/app?panel=catalog&details=whisper-base-q8_0");
    expect(detailsUrl(href, null)).toBe("/app?panel=catalog&q=coder");
  });

  test("restores requested model on Back even after the local conversation changed models", () => {
    const before = conversationNavigation(saved, null);
    const changed = { ...saved, model: "image:other" };
    const restored = resolveNavigation({
      navigation: before,
      conversations: [changed],
      fresh,
    });
    expect(restored.conversation.model).toBe(saved.model);
    expect(restored.conversation.messages).toBe(saved.messages);
  });
});

describe("History API navigation", () => {
  test("obsolete key fragments are discarded without disturbing navigation", () => {
    const location = new URL(
      "https://local.test/app?view=lab&mode=image&keep=yes#key=lb_secret",
    );
    const state = { unrelated: "state" };
    const writes: string[] = [];
    const history = {
      state,
      replaceState(data: unknown, _unused: string, url?: string | URL | null) {
        expect(data).toBe(state);
        location.href = new URL(String(url), location).href;
        writes.push(location.href);
      },
      pushState(data: unknown, unused: string, url?: string | URL | null) {
        this.replaceState(data, unused, url);
      },
    };
    discardLegacyFragmentCredential({ location, history });
    expect(readNavigation(location.search).mode).toBe("image");
    writeNavigation(conversationNavigation(saved, "settings"), "push", {
      location,
      history,
    });
    expect(location.searchParams.has("keep")).toBe(false);
    expect(writes.every((url) => !url.includes("secret"))).toBe(true);
  });

  test("modal open and close push once each; Back and Forward restoration do not push or loop", () => {
    const location = new URL("https://local.test/app");
    const entries = [location.href];
    let cursor = 0;
    let pushes = 0;
    const history = {
      state: null,
      replaceState(_data: unknown, _unused: string, url?: string | URL | null) {
        location.href = new URL(String(url), location).href;
        entries[cursor] = location.href;
      },
      pushState(_data: unknown, _unused: string, url?: string | URL | null) {
        location.href = new URL(String(url), location).href;
        entries.splice(++cursor, entries.length, location.href);
        pushes++;
      },
    };
    const environment = { location, history };
    writeNavigation(
      conversationNavigation(saved, null),
      "replace",
      environment,
    );
    writeNavigation(
      conversationNavigation(saved, "models"),
      "push",
      environment,
    );
    writeNavigation(
      conversationNavigation(saved, "models"),
      "push",
      environment,
    );
    writeNavigation(conversationNavigation(saved, null), "push", environment);
    expect(pushes).toBe(2);
    for (const [index, panel] of [
      [1, "models"],
      [0, null],
      [1, "models"],
      [2, null],
    ] as const) {
      cursor = index;
      location.href = entries[cursor];
      const resolved = resolveNavigation({
        navigation: readNavigation(location.search),
        conversations: [saved],
        fresh,
      });
      expect(resolved.navigation.panel).toBe(panel);
      writeNavigation(resolved.navigation, "replace", environment);
    }
    expect(pushes).toBe(2);
    expect(entries).toHaveLength(3);
  });
});

describe("startConversation", () => {
  let next = 0;
  const fresh = (mode: Mode, workspace: Conversation["workspace"]) => ({
    id: `new-${++next}`,
    title: "New conversation",
    workspace,
    mode,
    model: "m",
    messages: [],
  });
  const used = (id: string): Conversation => ({
    ...fresh("llm", "chat"),
    id,
    title: id,
    messages: [{ id: `${id}-m`, role: "user", text: "hi" }],
  });

  test("repeated workspace switches do not accumulate empty conversations", () => {
    let conversations = [used("a")];
    for (const [mode, workspace] of [
      ["llm", "chat"],
      ["embedding", "lab"],
      ["llm", "chat"],
      ["embedding", "lab"],
      ["llm", "chat"],
    ] as const) {
      conversations = startConversation({
        conversations,
        mode,
        workspace,
        fresh,
      }).conversations;
    }
    expect(conversations.filter((item) => !item.messages.length)).toHaveLength(
      1,
    );
    expect(conversations.map((item) => item.id)).toContain("a");
    expect(conversations[0]?.mode).toBe("llm");
  });

  test("reuses an empty conversation with the same mode and workspace", () => {
    const empty = { ...fresh("llm", "chat"), id: "empty" };
    const result = startConversation({
      conversations: [used("a"), empty],
      mode: "llm",
      workspace: "chat",
      fresh,
    });
    expect(result.conversation.id).toBe("empty");
    expect(result.conversations.map((item) => item.id)).toEqual(["empty", "a"]);
  });

  test("drops other empty conversations when starting a new one", () => {
    const result = startConversation({
      conversations: [{ ...fresh("tts", "lab"), id: "stale" }, used("a")],
      mode: "llm",
      workspace: "chat",
      fresh,
    });
    expect(result.conversation.id).not.toBe("stale");
    expect(result.conversations.map((item) => item.id)).toEqual([
      result.conversation.id,
      "a",
    ]);
  });
});

test("loading a URL without saved history reuses the empty conversation", () => {
  const empty: Conversation = {
    id: "empty",
    workspace: "chat",
    mode: "llm",
    model: "",
    title: "New conversation",
    messages: [],
  };
  const restored = resolveNavigation({
    navigation: readNavigation("?view=chat&mode=llm&model=llm%3Aqwen"),
    conversations: [empty, saved],
    fresh,
  });
  expect(restored.conversation.id).toBe("empty");
  expect(restored.conversation.model).toBe("llm:qwen");
  expect(restored.conversations.map((item) => item.id)).toEqual([
    "empty",
    saved.id,
  ]);
});
