# Chat-specific sharing controls

Objective: make shared documents/repositories/reviews visibly belong to the selected conversation rather than all chats.

Implemented: move sharing controls out of the global sidebar into a wrapping row immediately below the conversation title, labeled “In this chat.” Key all panels by chat ID so modal state cannot carry across conversations. Sidebar contains conversation navigation and account controls.

Remaining: build, deploy web assets on OVH without interrupting agents, visually verify, commit/push and clean worktree.

Completed: production build/typecheck passed.3917acd deployed on OVH by recreating only idle chat service; runner/policy/edge remained running. Verified in signed-in system Chrome: selected conversation title immediately precedes the “In this chat” toolbar; sidebar has only chat navigation/account controls. Committed/pushed origin/codex/warden-chat-controls. Source preserved at workspace .local/warden-chat-controls-source; clean worktree removed. No remaining work.
