import { Route, Routes } from "react-router";
import Shell from "./ui/Shell";
import PageTitle from "./ui/PageTitle";
import Submit from "./views/Submit";
import Job from "./views/Job";

function Corpus() {
  return (
    <>
      <PageTitle>Corpus</PageTitle>
      <p>The repositories codetrail has indexed.</p>
    </>
  );
}

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
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}
