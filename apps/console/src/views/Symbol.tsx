import { useEffect, useState } from "react";
import { useParams, useSearchParams } from "react-router";
import { callersOf, getSymbol } from "../api/client";
import type { Callers as CallersValue, Outcome, SymbolRead } from "../api/types";
import PageTitle from "../ui/PageTitle";
import Citation from "../ui/Citation";
import Staleness from "../ui/Staleness";
import CallersList from "../ui/Callers";
import ErrorPanel from "../ui/ErrorPanel";

const DEPTHS = [1, 2, 3, 4, 5];

export default function SymbolView() {
  const { repo = "", symbol = "" } = useParams();
  const [params, setParams] = useSearchParams();
  // Read from the URL, not clamped. graph.go:358-361 answers a 400 naming the
  // bound rather than silently serving 5, and a client that clamps re-hides
  // exactly what that refuses to hide.
  const depth = Number(params.get("depth") ?? "1");
  const [def, setDef] = useState<Outcome<SymbolRead> | null>(null);
  const [callers, setCallers] = useState<Outcome<CallersValue> | null>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const [d, c] = await Promise.all([
        getSymbol(repo, symbol),
        callersOf(repo, symbol, { depth }),
      ]);
      if (!cancelled) {
        setDef(d);
        setCallers(c);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [repo, symbol, depth]);

  return (
    <>
      <PageTitle>Definition</PageTitle>
      {def?.kind === "ok" && (
        <>
          <p>
            <code>{def.value.symbol.name}</code> ({def.value.symbol.kind}) in{" "}
            <code>{def.value.symbol.pkg}</code>
          </p>
          <Citation citation={def.value.citation} symbol={def.value.symbol} check="open" />
          <Staleness staleness={def.value.staleness} />
        </>
      )}

      <p>
        <label htmlFor="depth">Depth</label>
        {/* 1 to 5, because the server refuses anything else with a 400 and the
            console must not offer a value it knows will be refused. */}
        <select
          id="depth"
          value={String(depth)}
          onChange={(e) => setParams({ depth: e.target.value })}
        >
          {DEPTHS.map((d) => (
            <option key={d} value={d}>
              {d}
            </option>
          ))}
        </select>
      </p>

      {callers?.kind === "ok" && <CallersList callers={callers.value} />}
      {callers && callers.kind !== "ok" && (
        <ErrorPanel
          title="codetrail could not answer who calls this."
          detail={callers.detail}
          requestId={callers.kind === "failed" ? callers.requestId : null}
        />
      )}
      {def && def.kind !== "ok" && callers?.kind === "ok" && (
        <ErrorPanel
          title="codetrail could not read this definition."
          detail={def.detail}
          requestId={def.kind === "failed" ? def.requestId : null}
        />
      )}
    </>
  );
}
