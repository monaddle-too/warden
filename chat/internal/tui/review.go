package tui

import (
	"fmt"
	"strconv"
)

// Reviews: the agent's requests only the app can settle (a pull request
// proposal, suggested document edits, a document selection or creation)
// are listed on the chat while they wait; the card names them and /review
// opens the app on the chat, where they are done. Nothing here answers
// one.

// review is /review [N]: open the app on this chat for its Nth review
// (the first without N). Without a browser, or when opening fails, the
// URL is shown so the person can open it themselves.
func (a *App) review(c *Chat, arg string) {
	if c == nil {
		a.setNotice("no chat selected")
		return
	}
	if len(c.Reviews) == 0 {
		a.setNotice("nothing to review: the agent has not proposed a pull request, document suggestions or a document choice")
		return
	}
	n := 1
	if arg != "" {
		v, err := strconv.Atoi(arg)
		if err != nil || v < 1 || v > len(c.Reviews) {
			a.setNotice(fmt.Sprintf("/review N with N from 1 to %d", len(c.Reviews)))
			return
		}
		n = v
	}
	r := c.Reviews[n-1]
	if a.AppURL == "" {
		a.setNotice(r.Summary(c.Provider) + " — open the Warden app on this chat to review it")
		return
	}
	url := PopupURL(a.AppURL, c.ID)
	if a.OpenURL == nil {
		a.setNotice("review it in the app: " + url)
		return
	}
	if err := a.OpenURL(url); err != nil {
		a.setNotice("could not open the browser (" + err.Error() + "); review it in the app: " + url)
		return
	}
	a.setNotice("opened the app on this chat: " + r.Summary(c.Provider))
}
