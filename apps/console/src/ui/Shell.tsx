import { createContext, useState } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router";

// True once this session has navigated at least once. A page load is not a
// navigation: focusing the h1 on first paint would put focus after the skip
// link, which is the one thing the skip link exists to be before.
export const RouteChanged = createContext(false);

export default function Shell() {
  const location = useLocation();
  // State, not a ref: the first key has to be readable during render, and
  // reading a ref there is what react-hooks/refs forbids.
  const [firstKey] = useState(location.key);
  const changed = location.key !== firstKey;

  return (
    <RouteChanged.Provider value={changed}>
      <a className="skip" href="#main">
        Skip to content
      </a>
      {/* One row, brand then nav. The screenshot this replaces read "HomeCorpus"
          — two links with no border, no colour and no gap between them. */}
      <div className="bar">
        <header>
          <Link to="/">codetrail</Link>
        </header>
        <nav aria-label="Corpus">
          <NavLink to="/">Home</NavLink>
          <NavLink to="/repos">Corpus</NavLink>
        </nav>
      </div>
      <main id="main" tabIndex={-1}>
        <Outlet />
      </main>
    </RouteChanged.Provider>
  );
}
