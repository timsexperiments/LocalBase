import { expect, test } from "bun:test";
import type { Permission } from "../auth/authorization";
import { gatewayAuthorizationRequirement } from "./route-authorization";
import { selectGatewayRoute } from "./route-dispatch";

const job = "/v1/videos/00000000-0000-4000-8000-000000000000";
const protectedRoutes: {
  path: string;
  method: string;
  permission: Permission;
  alwaysRequired: boolean;
}[] = [
  {
    path: "/v1/models",
    method: "GET",
    permission: "models:read",
    alwaysRequired: false,
  },
  {
    path: "/_localbase/models",
    method: "GET",
    permission: "models:read",
    alwaysRequired: true,
  },
  {
    path: "/_localbase/models/qwen%2Ftest",
    method: "GET",
    permission: "models:read",
    alwaysRequired: true,
  },
  {
    path: "/_localbase/access-management",
    method: "GET",
    permission: "access:read",
    alwaysRequired: true,
  },
  {
    path: "/_localbase/api-keys",
    method: "GET",
    permission: "keys:read",
    alwaysRequired: true,
  },
  {
    path: "/_localbase/api-keys",
    method: "POST",
    permission: "keys:manage",
    alwaysRequired: true,
  },
  {
    path: "/v1/chat/completions",
    method: "POST",
    permission: "inference:chat",
    alwaysRequired: false,
  },
  {
    path: "/v1/embeddings",
    method: "POST",
    permission: "inference:embeddings",
    alwaysRequired: false,
  },
  {
    path: "/v1/audio/transcriptions",
    method: "POST",
    permission: "inference:transcription",
    alwaysRequired: false,
  },
  {
    path: "/v1/audio/translations",
    method: "POST",
    permission: "inference:transcription",
    alwaysRequired: false,
  },
  {
    path: "/v1/audio/speech",
    method: "POST",
    permission: "inference:speech",
    alwaysRequired: false,
  },
  {
    path: "/v1/images/generations",
    method: "POST",
    permission: "inference:image",
    alwaysRequired: false,
  },
  {
    path: "/v1/videos",
    method: "POST",
    permission: "inference:video",
    alwaysRequired: true,
  },
  {
    path: job,
    method: "GET",
    permission: "inference:video",
    alwaysRequired: true,
  },
  {
    path: job,
    method: "DELETE",
    permission: "inference:video",
    alwaysRequired: true,
  },
  {
    path: `${job}/content`,
    method: "GET",
    permission: "inference:video",
    alwaysRequired: true,
  },
  {
    path: `${job}/cancel`,
    method: "POST",
    permission: "inference:video",
    alwaysRequired: true,
  },
];

test.each(protectedRoutes)(
  "maps $method $path to $permission",
  ({ path, method, permission, alwaysRequired }) => {
    const route = selectGatewayRoute(path);
    for (const authRequired of [true, false]) {
      expect(
        gatewayAuthorizationRequirement({ route, method, authRequired }),
      ).toEqual(
        authRequired || alwaysRequired
          ? { kind: "permission", permission }
          : { kind: "public" },
      );
    }
  },
);

test.each(["/health", "/health/ready", "/_localbase/instance"])(
  "leaves %s outside API-key authorization for all methods",
  (path) => {
    for (const authRequired of [true, false]) {
      for (const method of ["GET", "HEAD", "POST", "PATCH"]) {
        expect(
          gatewayAuthorizationRequirement({
            route: selectGatewayRoute(path),
            method,
            authRequired,
          }),
        ).toEqual({ kind: "public" });
      }
    }
  },
);

test("preflight is public on protected and unknown paths", () => {
  for (const path of [
    ...protectedRoutes.map((entry) => entry.path),
    "/unexposed",
  ]) {
    for (const authRequired of [true, false]) {
      expect(
        gatewayAuthorizationRequirement({
          route: selectGatewayRoute(path),
          method: "OPTIONS",
          authRequired,
        }),
      ).toEqual({ kind: "public" });
    }
  }
});

test("nonstandard methods retain authorization requirements", () => {
  for (const { path, permission } of protectedRoutes.filter(
    ({ path }) =>
      path !== "/_localbase/access-management" &&
      path !== "/_localbase/api-keys",
  )) {
    expect(
      gatewayAuthorizationRequirement({
        route: selectGatewayRoute(path),
        method: "PATCH",
        authRequired: true,
      }),
    ).toEqual({ kind: "permission", permission });
  }
});

test("defers access-management POST actions to the body-aware handler", () => {
  for (const authRequired of [true, false]) {
    expect(
      gatewayAuthorizationRequirement({
        route: selectGatewayRoute("/_localbase/access-management"),
        method: "POST",
        authRequired,
      }),
    ).toEqual({ kind: "authenticated" });
  }
});

test.each([
  "/unexposed",
  "/health/",
  "/v1/videos/invalid",
  "/_localbase/models/%ZZ",
])("preserves the authentication gate for unknown path %s", (path) => {
  const route = selectGatewayRoute(path);
  expect(
    gatewayAuthorizationRequirement({
      route,
      method: "GET",
      authRequired: true,
    }),
  ).toEqual({ kind: "authenticated" });
  expect(
    gatewayAuthorizationRequirement({
      route,
      method: "GET",
      authRequired: false,
    }),
  ).toEqual({ kind: "public" });
});
