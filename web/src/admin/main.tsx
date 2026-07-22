import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./admin.css";

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <App apiPath={root.dataset.api ?? "/__anteroom/admin/api/"} />
    </StrictMode>,
  );
}
