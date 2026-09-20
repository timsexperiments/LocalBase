import { describe, expect, test } from "bun:test";
import {
  discardLegacyFragmentCredential,
  type Conversation,
  type Mode,
} from "./client";
import {
  conversationNavigation,
  navigationUrl,
  readNavigation,
  resolveNavigation,
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
    ] as const) {
      for (const panel of [
        null,
        "models",
        "settings",
        "history",
        "generation",
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
