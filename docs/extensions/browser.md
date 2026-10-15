# Browser extension

The `browser` extension (`extensions/browser/`) lets agents drive a real Chromium over the Chrome DevTools Protocol (CDP). An agent opens a page, reads a numbered list of its interactive elements, and clicks, types or selects by number. It can also ask the decision model which element fits its goal.

```yaml
extensions:
  browser:
    cdp_url: ws://chromium:3000          # required: the browser-level DevTools websocket, e.g. a browserless container
    allowed_hosts: [example.com]         # exact hostnames a page may load; empty (the default) refuses every page
    max_elements: 50                     # cap on an observation's element list; default 50
```

`enabled`/`data_dir`/`exec` are the SDK-level keys every extension block accepts (see the [extensions README](../../extensions/README.md)). quack dials `cdp_url` on the first tool call, not at boot, and dials again after the connection drops. Tabs opened on a dropped connection are gone; open the page again.

## Tools

| Tool | What it does |
| --- | --- |
| `browser_open(url)` | Opens `url` in a new tab if its host is allowed and returns the page id (`p1`, `p2`, ...) with an observation. |
| `browser_observe(page)` | Returns the page's title, URL, numbered elements and screenshot. |
| `browser_act(page, element, action, text?)` | `click`, `type` (replaces the field's value with `text`) or `select` (the option whose label or value is `text`) on a numbered element, then returns the new observation. |
| `browser_choose(page, goal)` | Asks the `element.choose` decision point which element of the latest observation to act on, and returns its number, label, probability and `clears_act_at`. The agent still calls `browser_act`. |

An observation lists the visible interactive elements in document order as `[n] role "accessible name"`. Each element's role and name come from its ARIA attributes, label or text. Only the latest observation's numbers are valid: `browser_act` refuses a number outside it, and an element the page has since removed fails with "call browser_observe again". The screenshot is a 1280x800 JPEG stored as the chat artifact `bytes:browser-<page>.jpg`, one revision per observation; the result carries its id and revision, never the bytes. A page belongs to the chat that opened it.

## Decision point

`ext:browser/element.choose` is observe-only. Its one question, `element` (choice, primary), asks which element to click or fill next to reach the goal in the state. Its options are the observation's element labels, sent per call. The state is the goal and the page's title and URL, and the screenshot goes beside it as an image. The ledger records the state without the image. `browser_choose` has no baseline, so `quack decisions report` shows no agreement for it. `clears_act_at` is the recorded `confident`: whether the top probability reached the point's `act_at`.

```yaml
decisions:
  points:
    "ext:browser/element.choose": { enabled: true, handler: clef }
```

## Security

`allowed_hosts` is the only thing between an agent and the open web, so it defaults to denying everything. Hosts match exactly: `example.com` does not allow `www.example.com`. Only `http` and `https` URLs load, plus `about:blank`. Each tab intercepts its document requests (top-level pages, redirects and frames) and fails any to a host off the list before it is sent. After every navigation and action quack also re-reads the tab's URL; a page that ended up off the list is reset to `about:blank` and the call fails. Subresources (images, scripts, XHR) are not filtered, so an allowed page can still load from or post to other hosts. A popup the page opens in a new tab is not intercepted either. Point `cdp_url` at a browser dedicated to quack.
