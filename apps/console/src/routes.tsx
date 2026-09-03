import { Route, Routes } from "react-router";
import Shell from "./ui/Shell";
import PageTitle from "./ui/PageTitle";
import Submit from "./views/Submit";
import Corpus from "./views/Corpus";
import Job from "./views/Job";
import Ask from "./views/Ask";
import Span from "./views/Span";
import Symbols from "./views/Symbols";
import SymbolView from "./views/Symbol";

// Its own h1, not a blank main: a route that renders nothing is
// indistinguishable from a view that failed to load.
function NotFound() {
  return (
    <>
      <PageTitle>Page not found</PageTitle>
      <p>codetrail has no page at this address.</p>
    </>
  );
}

export default function AppRoutes() {
  return (
    <Routes>
      <Route element={<Shell />}>
        <Route index element={<Submit />} />
        <Route path="repos" element={<Corpus />} />
        <Route path="jobs/:id" element={<Job />} />
        <Route path="repos/:repo" element={<Ask />} />
        <Route path="repos/:repo/spans/:span" element={<Span />} />
        <Route path="repos/:repo/symbols" element={<Symbols />} />
        <Route path="repos/:repo/symbols/:symbol" element={<SymbolView />} />
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}
