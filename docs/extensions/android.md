# Android extension

The `android` extension (`extensions/android/`) lets agents drive an Android emulator or device through an adb server. An agent reads a numbered list of the screen's interactive elements, then taps, types or long-presses by number, presses back, swipes, or launches an app. It can also ask the decision model which element fits its goal.

```yaml
extensions:
  android:
    adb_addr: emulator:5037                         # required: the adb server's host:port
    serial: emulator-5554                           # optional: which device; default the adb server's only one
    allowed_packages: [com.android.settings]        # apps an agent may act inside or launch; empty (the default) refuses every action
    max_elements: 50                                # cap on an observation's element list; default 50
```

`enabled`/`data_dir`/`exec` are the SDK-level keys every extension block accepts (see the [extensions README](../../extensions/README.md)). quack speaks the adb host protocol to `adb_addr` directly, one TCP connection per device command, so the quack image needs no `adb` binary and nothing is dialed at boot. An unreachable server or a missing device fails the tool call.

## Tools

| Tool | What it does |
| --- | --- |
| `android_observe()` | Returns the foreground package and activity, the numbered elements and a screenshot. Works in any app, so an agent can see where it is. |
| `android_act(element?, action, text?, direction?)` | `tap`, `type` (taps the element, then types `text` at the cursor) or `long_press` a numbered element; or `back`, or `swipe` with `direction` `up`, `down`, `left` or `right` (the way the finger moves). Returns the new observation. |
| `android_launch(package)` | Starts an allowed app's launcher activity and returns the new observation. |
| `android_choose(goal)` | Asks the `element.choose` decision point which element of the latest observation to act on, and returns its number, label, probability and `clears_act_at`. The agent still calls `android_act`. |

The elements come from a `uiautomator dump` of the foreground window, in document order, as `[n] Class "label"`. A node is listed when it is enabled, not a scrolling container (swipe scrolls), has a non-empty box, and is clickable, long-clickable, focusable or an `EditText`. Its label is its own text or content description, else its first descendant's (a list row's title), else its resource id. A tap lands at the center of the element's bounds. Only the latest observation's numbers are valid, and only for the chat that made it: `android_act` re-reads the screen first and refuses an element that has moved or gone, which also voids the observation until the next `android_observe`. The screenshot is the screen as a JPEG, stored as the chat artifact `bytes:android-screen.jpg`, one revision per observation; the result carries its id and revision, never the bytes. Each action waits 700 ms for the UI to settle before observing. `type` sends `input text`, which handles ASCII only.

## Decision point

`ext:android/element.choose` is observe-only. Its one question, `element` (choice, primary), asks which element to tap or type into next to reach the goal in the state. Its options are the observation's element labels, sent per call. The state is the goal and the foreground package and activity, and the screenshot goes beside it as an image. The ledger records the state without the image. `android_choose` has no baseline, so `quack decisions report` shows no agreement for it. `clears_act_at` is the recorded `confident`: whether the top probability reached the point's `act_at`.

```yaml
decisions:
  points:
    "ext:android/element.choose": { enabled: true, handler: clef }
```

## Security

`allowed_packages` is the only thing between an agent and everything else on the device, so it defaults to refusing every action. Packages match exactly. `android_act` checks the foreground package (from `dumpsys window`'s focused window) before it touches the screen and refuses outside the list. After every action and launch it checks again: a screen that ended up in another app, such as a link that opened a browser or a permission dialog, makes the call fail and presses Home. Add a system app such as `com.google.android.permissioncontroller` to the list if the flow needs it. `android_launch` takes only listed packages. Observing is allowed everywhere, so a screenshot or element list can show another app's content. Anything the agent types reaches the device's shell as a single-quoted `input text` argument. Point `adb_addr` at an emulator dedicated to quack: the adb server gives whoever reaches it a shell on every device it serves, so keep port 5037 off untrusted networks.

## Emulator setup

quack needs an adb server it can reach over TCP with one emulator attached. An emulator container such as [docker-android](https://github.com/budtmo/docker-android) runs both. It needs KVM, so run it on a Linux host with `/dev/kvm`, and publish its adb server port only where quack runs:

```yaml
services:
  android:
    image: budtmo/docker-android:emulator_14.0
    devices: [/dev/kvm]
    environment:
      EMULATOR_DEVICE: Samsung Galaxy S10
    ports:
      - "127.0.0.1:5037:5037"
```

An adb server listens on localhost only unless started with `-a`, so make sure the container's runs as `adb -a -P 5037 nodaemon server`; otherwise the published port reaches nothing. Then set `adb_addr: 127.0.0.1:5037`, or the service name and port when quack shares the compose network. `adb -H 127.0.0.1 -P 5037 devices` should list one device; set `serial` when it lists several. The first boot takes a few minutes, and tool calls fail until the device is up.
