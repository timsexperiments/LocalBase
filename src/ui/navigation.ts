import { z } from "zod";
import { modes, type Conversation, type Mode } from "./client";

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
    .enum(["models", "settings", "history", "generation"])
    .nullable()
    .catch(null),
  conversation: identifier,
});
export type Navigation = z.infer<typeof navigationSchema>;

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
  const conversation = saved
    ? { ...saved, model: navigation.model || saved.model }
    : { ...fresh(navigation.mode, navigation.view), model: navigation.model };
  return {
    conversation,
    conversations: saved
      ? conversations.map((item) =>
          item.id === saved.id ? conversation : item,
        )
      : [conversation, ...conversations].slice(0, 30),
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
