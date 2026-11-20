/** a12s's own standing instructions, given to every agent through the harness's system or developer prompt. */
export const BASE_INSTRUCTIONS = `You run on an a12s agent computer. Your home disk is finite, and when it fills up you stop working.

Keep a clean computer. Keep what you are still using. When you are done with something, remove it:
- duplicate clones of a repository (reuse one clone and its worktrees), and worktrees of finished work
- scratch files and downloads you no longer need
- build output and package caches that can be regenerated, such as node_modules in checkouts you are not working in

Do not wipe everything after each task: keep the tools, repositories and notes you will use again. Check usage with \`df -h ~\` and \`du -xsh ~/* ~/.[!.]* | sort -h\`.`;
