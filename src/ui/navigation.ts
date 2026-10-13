import { z } from "zod";
import type { Permission } from "../domains/auth/authorization";
import { hasPermission, modes, type Conversation, type Mode } from "./client";

const identifier = z
  .string()
  .min(1)
  .max(512)
  .regex(/^[^\s\x00-\x1f\x7f]+$/)
  .catch("");
const navigationSchema = z.object({
  view: z.enum(["chat", "lab"]).catch("chat"),
  mode: z.enum(modes).catch("llm"),
  model: identifier,
  panel: z
    .enum(["models", "settings", "history", "generation", "catalog", "admin"])
    .nullable()
    .catch(null),
  conversation: identifier,
});
export type Navigation = z.infer<typeof navigationSchema>;

const panelPermissions: Partial<
  Record<NonNullable<Navigation["panel"]>, Permission>
> = {
  catalog: "models:manage",
};

export type AdministrationSection = "people" | "access" | "keys";

export function administrationSections(
  permissions: readonly Permission[] | null,
): AdministrationSection[] {
  return [
    ...(hasPermission(permissions, "access:read")
      ? (["people", "access"] as const)
      : []),
    ...(hasPermission(permissions, "keys:read") ? (["keys"] as const) : []),
  ];
}

export function canOpenPanel(
  panel: Navigation["panel"],
  permissions: readonly Permission[] | null,
): boolean {
  if (panel === "admin") return administrationSections(permissions).length > 0;
  const required = panel ? panelPermissions[panel] : undefined;
  return !required || hasPermission(permissions, required);
}

export function readNavigation(search: string): Navigation {
  const params = new URLSearchParams(search);
  const parsed = navigationSchema.parse(
    Object.fromEntries(
      ["view", "mode", "model", "panel", "conversation"].map((key) => [
        key,
        params.get(key),
      ]),
    ),
  );
  return { ...parsed, mode: parsed.view === "chat" ? "llm" : parsed.mode };
}

export function conversationNavigation(
  conversation: Conversation,
  panel: Navigation["panel"],
  model = conversation.model,
): Navigation {
  return {
    view: conversation.workspace,
    mode: conversation.mode,
    model,
    panel,
    conversation: conversation.id,
  };
}

export function resolveNavigation({
  navigation,
  conversations,
  fresh,
}: {
  navigation: Navigation;
  conversations: Conversation[];
  fresh: (mode: Mode, workspace: Conversation["workspace"]) => Conversation;
}) {
  // A URL can name local history, but cannot supply or reinterpret its contents.
  const saved = conversations.find(
    (item) => item.id === navigation.conversation,
  );
  if (!saved) {
    // Reuse an empty conversation rather than adding another on each visit.
    const started = startConversation({
      conversations,
      mode: navigation.mode,
      workspace: navigation.view,
      fresh,
    });
    const conversation = {
      ...started.conversation,
      model: navigation.model || started.conversation.model,
    };
    return {
      conversation,
      conversations: started.conversations.map((item) =>
        item.id === conversation.id ? conversation : item,
      ),
      navigation: conversationNavigation(conversation, navigation.panel),
    };
  }
  const conversation = { ...saved, model: navigation.model || saved.model };
  return {
    conversation,
    conversations: conversations.map((item) =>
      item.id === saved.id ? conversation : item,
    ),
    navigation: conversationNavigation(conversation, navigation.panel),
  };
}

export function navigationUrl(href: string, navigation: Navigation): string {
  const url = new URL(href);
  const search = new URLSearchParams();
  for (const key of [
    "view",
    "mode",
    "model",
    "panel",
    "conversation",
  ] as const) {
    const value = navigation[key];
    if (value) search.set(key, value);
  }
  // Fragment credentials are consumed before navigation is initialized.
  const query = search.toString();
  return url.pathname + (query ? `?${query}` : "");
}

export function writeNavigation(
  navigation: Navigation,
  method: "push" | "replace",
  {
    location,
    history,
  }: {
    location: Pick<Location, "href" | "pathname" | "search">;
    history: Pick<History, "state" | "pushState" | "replaceState">;
  } = window,
) {
  const url = navigationUrl(location.href, navigation);
  if (url === location.pathname + location.search) return;
  history[method === "push" ? "pushState" : "replaceState"](
    history.state,
    "",
    url,
  );
}

/**
 * Opens a blank conversation for a mode and workspace. An existing empty
 * conversation for the same mode and workspace is reused, and other empty
 * conversations are dropped, so switching workspaces never piles up blank
 * "New conversation" entries in history.
 */
export function startConversation({
  conversations,
  mode,
  workspace,
  fresh,
}: {
  conversations: Conversation[];
  mode: Mode;
  workspace: Conversation["workspace"];
  fresh: (mode: Mode, workspace: Conversation["workspace"]) => Conversation;
}) {
  const conversation =
    conversations.find(
      (item) =>
        item.messages.length === 0 &&
        item.mode === mode &&
        item.workspace === workspace,
    ) ?? fresh(mode, workspace);
  return {
    conversation,
    conversations: [
      conversation,
      ...conversations.filter(
        (item) => item.id !== conversation.id && item.messages.length > 0,
      ),
    ].slice(0, 30),
  };
}
