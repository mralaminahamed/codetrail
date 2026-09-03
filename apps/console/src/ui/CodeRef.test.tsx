import { describe, expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import CodeRef, { checkCommand } from "./CodeRef";
import span from "../api/fixtures/span.json";

describe("the runnable check", () => {
  test("the command is composed from the commit, path and line range", () => {
    const c = span.citation;
    expect(checkCommand(c.commit, c.path, c.start_line, c.end_line)).toBe(
      `git show ${c.commit}:calc/calc.go | sed -n '19,23p' | head -c -1 | sha256sum`,
    );
  });

  test("head -c -1 is in the command, because a span's text ends at the last byte of its last line", () => {
    // Not a stylistic nicety: without it sha256sum hashes one byte more than
    // the digest covers and every check a reader runs fails.
    expect(checkCommand("abc", "a.go", 1, 2)).toContain("| head -c -1 | sha256sum");
  });

  test("the expected digest is rendered beside the command", () => {
    const c = span.citation;
    render(
      <CodeRef commit={c.commit} path={c.path} startLine={c.start_line} endLine={c.end_line} digest={c.digest} />,
    );
    expect(screen.getByText(checkCommand(c.commit, c.path, c.start_line, c.end_line))).toBeInTheDocument();
    expect(screen.getByText(c.digest)).toBeInTheDocument();
  });
});
