import { createRoot } from "react-dom/client";
import { AuthRoot } from "./components/AuthRoot";
import "./tokens.css";
import "./base.css";
import "./chat.css";
import "./conversation.css";
createRoot(document.getElementById("root")!).render(<AuthRoot />);
