import "@testing-library/jest-dom/vitest";
import { setupServer } from "msw/node";
import { afterAll, afterEach, beforeAll, expect } from "vitest";
import { toHaveNoViolations } from "jest-axe";

expect.extend(toHaveNoViolations);

// Measured on vitest 3.2.7 + jsdom 27: fetch, Request, Response, Headers,
// TransformStream and BroadcastChannel are all already functions here, so MSW
// v2 needs no polyfill and none is installed.
export const server = setupServer();

// "error", not "bypass": a request no handler matches is a test asserting
// against a live network, and it must fail rather than pass quietly.
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
