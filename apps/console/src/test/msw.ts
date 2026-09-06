import { delay, http, HttpResponse } from "msw";
import { server } from "./setup";

// What the handler saw. A count and the parsed bodies, because the mutations
// this phase runs are about *whether a request was made* and *which keys it
// carried* — neither of which a response assertion can see.
export type Recorder = {
  calls: number;
  bodies: unknown[];
  urls: string[];
};

type Method = "get" | "post";

function record(method: Method, path: string, respond: (r: Recorder) => Response | Promise<Response>): Recorder {
  const rec: Recorder = { calls: 0, bodies: [], urls: [] };
  server.use(
    http[method](path, async ({ request }) => {
      rec.calls++;
      rec.urls.push(new URL(request.url).pathname + new URL(request.url).search);
      if (method === "post") {
        rec.bodies.push(await request.clone().json());
      }
      return respond(rec);
    }),
  );
  return rec;
}

export function stub(method: Method, path: string, status: number, body: unknown): Recorder {
  return record(method, path, () => HttpResponse.json(body as object, { status }));
}

// A handler that answers by what the request asked for. An unconditional 400
// cannot separate a client that forwards an out-of-range value from one that
// clamps it: both receive the same body and render the same sentence.
export function stubByQuery(
  method: Method,
  path: string,
  param: string,
  bad: (value: string | null) => boolean,
  onBad: { status: number; body: unknown },
  onGood: { status: number; body: unknown },
): Recorder {
  return record(method, path, (rec) => {
    const last = rec.urls[rec.urls.length - 1] ?? "";
    const value = new URLSearchParams(last.split("?")[1] ?? "").get(param);
    const pick = bad(value) ? onBad : onGood;
    return HttpResponse.json(pick.body as object, { status: pick.status });
  });
}

// A response that changes with each call, so a poller's sequence of states is
// drivable without re-registering a handler mid-test.
export function stubSequence(method: Method, path: string, bodies: unknown[]): Recorder {
  return record(method, path, (rec) => {
    const at = Math.min(rec.calls - 1, bodies.length - 1);
    return HttpResponse.json(bodies[at] as object, { status: 200 });
  });
}

// A response that takes a measurable moment. Without one, a pending state is
// unobservable in this suite: MSW answers within the same tick userEvent
// already awaits, so "is the previous result still on screen while the next
// request runs" has no window to be asked in.
export function stubSlow(method: Method, path: string, status: number, body: unknown, ms = 120): Recorder {
  return record(method, path, async () => {
    await delay(ms);
    return HttpResponse.json(body as object, { status });
  });
}

// A transport failure: no response at all, which is what a dropped connection,
// a dead process and a browser that refused the request all look like.
export function stubUnreachable(method: Method, path: string): Recorder {
  return record(method, path, () => HttpResponse.error());
}

// An error page that is not JSON. A proxy, a captive portal or a load balancer
// with no route produces this, and it must render as an error rather than a
// white screen.
export function stubHtml(method: Method, path: string, status: number): Recorder {
  return record(method, path, () =>
    new HttpResponse("<!doctype html><title>502</title><h1>Bad gateway</h1>", {
      status,
      headers: { "content-type": "text/html" },
    }),
  );
}
