import { useState } from "react";

// The one action this product exists to enable, and until now it required
// selecting a wrapped 64-character digest — or a 90-character shell command
// with a pipe in it — by hand, out of a block that had no borders and no
// spacing. A citation you can check is the claim; a citation you can copy is
// what makes checking it a thing a person actually does.
//
// There is NO text inside the button, and that is load-bearing twice over:
//
//  1. The button sits inside the <dd> whose value it copies, and the citation's
//     tests read that <dd>'s textContent as the exact tuple value. An <svg> has
//     no text nodes, so the dd still reads as the digest and nothing else.
//  2. The accessible name is on aria-label and changes with the outcome, so a
//     screen reader hears "Copied the digest" rather than nothing at all.
//
// The failure state is real and not defensive padding: navigator.clipboard is
// undefined outright on an insecure origin in Chrome, and this console has no
// deployment yet — every hand-run of it so far has been over plain http on
// localhost. A button that silently does nothing there is worse than no button.
type State = "idle" | "copied" | "failed";

const LABEL: Record<State, (what: string) => string> = {
  idle: (what) => `Copy ${what}`,
  copied: (what) => `Copied ${what}`,
  failed: (what) => `Could not copy ${what} — this browser refused the clipboard`,
};

function Glyph({ state }: { state: State }) {
  if (state === "copied") {
    return (
      <svg viewBox="0 0 16 16" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2">
        <path d="M2.5 8.5 6 12l7.5-8" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    );
  }
  if (state === "failed") {
    return (
      <svg viewBox="0 0 16 16" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="2">
        <path d="M8 3v6M8 12.5v.5" strokeLinecap="round" />
      </svg>
    );
  }
  return (
    <svg viewBox="0 0 16 16" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" strokeWidth="1.6">
      <rect x="5.2" y="5.2" width="8.3" height="8.3" rx="1.6" />
      <path d="M10.8 2.5H3.4a1 1 0 0 0-1 1v7.4" strokeLinecap="round" />
    </svg>
  );
}

export default function Copy({
  what,
  value,
  className = "copy",
}: {
  // Completes "Copy …" and "Copied …". A noun phrase, lower case.
  what: string;
  value: string;
  className?: string;
}) {
  const [state, setState] = useState<State>("idle");

  async function onClick() {
    const clip = navigator.clipboard;
    if (!clip) {
      setState("failed");
      return;
    }
    try {
      await clip.writeText(value);
      setState("copied");
    } catch {
      setState("failed");
    }
  }

  // Reset on leaving rather than on a timer. A timer here would be the only
  // scheduled work in the console and the only thing a test has to advance;
  // "the confirmation lasts as long as you are still on the button" needs
  // neither, and a stale tick is impossible because leaving clears it.
  return (
    <button
      type="button"
      className={className}
      data-copied={state === "copied"}
      data-failed={state === "failed"}
      aria-label={LABEL[state](what)}
      onClick={() => void onClick()}
      onBlur={() => setState("idle")}
      onPointerLeave={() => setState("idle")}
    >
      <Glyph state={state} />
    </button>
  );
}
