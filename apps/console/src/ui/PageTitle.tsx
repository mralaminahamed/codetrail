import { useContext, useEffect, useRef, type ReactNode } from "react";
import { RouteChanged } from "./Shell";

// tabIndex={-1} is what makes .focus() land: a heading is not focusable by
// default, so without it a route change leaves focus on the unmounted link and
// therefore on <body>.
export default function PageTitle({
  children,
  title,
}: {
  children: ReactNode;
  title?: string;
}) {
  const h1 = useRef<HTMLHeadingElement>(null);
  const changed = useContext(RouteChanged);
  const name = title ?? (typeof children === "string" ? children : "");

  useEffect(() => {
    if (changed) h1.current?.focus();
  }, [changed]);

  useEffect(() => {
    document.title = name ? `${name} — codetrail` : "codetrail";
  }, [name]);

  return (
    <h1 tabIndex={-1} ref={h1}>
      {children}
    </h1>
  );
}
