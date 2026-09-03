import type { ReactNode } from "react";

// polite, never assertive: indexing progress is not urgent, and an assertive
// region interrupts a screen-reader user mid-word on every poll. role="alert"
// is reserved for the error panel, where interrupting is the point.
//
// The accessible name is load-bearing: the answer outcome region is also a
// role="status", so a bare getByRole("status") in a tree holding both throws
// "found multiple elements" rather than failing on a claim.
export default function Live({ children }: { children: ReactNode }) {
  return (
    <div
      role="status"
      aria-live="polite"
      aria-atomic="true"
      aria-label="Indexing progress"
    >
      {children}
    </div>
  );
}
