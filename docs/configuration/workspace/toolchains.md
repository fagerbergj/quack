# Setting up toolchains

The runtime image ships **Go and Node only** — enough for the trust gate's deterministic checks to `go build` or `npm test` what a code-implementer writes (see the [`Dockerfile`](../../../Dockerfile)). Any other language has to be supplied at the workspace level; baking every possible toolchain into the image doesn't scale. The baked Go/Node stay the default either way — a workspace toolchain supplements or overrides them, it doesn't replace them.

## How `exec_path` and `env` work

`exec_path` puts directories on the `PATH` every `run_command`/check/git child sees — and, under `sandbox: bwrap`, into the child's *filesystem*. `env` hands those same children, plus the ACP coding-agent subprocess (`opencode`), extra environment variables. Toolchains routinely need both: `PATH` alone finds `javac`, but Gradle also needs `JAVA_HOME`, and the Android Gradle Plugin needs `ANDROID_HOME` — a directory to look things up *in*, not a command to run.

**Precedence.** `workspace.env` is deployment-wide. An agent's own `acp: {env: ...}` (see [agents.md](../agents.md)) is more specific and wins on a shared key — e.g. one code-implementer pinned to a different JDK than the deployment default. `PATH` and `HOME` are reserved in `workspace.env`: they have dedicated knobs (`exec_path`, and the jail's isolated per-user home), and setting them here is a startup error rather than a silent override.

**Under `sandbox: bwrap`**, a directory an `env` value *points at* must be independently reachable inside the sandbox's mount namespace, or the toolchain "exists" by env var but not on disk (`JAVA_HOME` set, but `$JAVA_HOME/bin/java: No such file or directory`). `exec_path` entries are bind-mounted read-only verbatim, so list the toolchain root itself, not just its `bin/`, whenever `env` points at it. This is not a secrets mechanism: values interpolate `${VAR}` like the rest of the config, but an actual credential belongs in a provider or tool's own `auth:` block.

The shape is always the same:

1. Put the toolchain on a volume the container can read.
2. Point `workspace.exec_path` at its `bin` directories and `workspace.env` at the roots it needs to *find*.
3. Add the project's build command to `workspace.check_commands` so the gate can actually verify a change.
4. Verify by running the project's real test command inside the container.

## Provisioning the volume

Keep toolchains in their own volume, outside `workspace.root` — a path nested under the workspace root can collide with the sandbox's per-node mount. Populate it with throwaway containers rather than by hand:

```bash
docker volume create quack-toolchains

# A JDK: copy it straight out of an official image.
docker run --rm -v quack-toolchains:/tc eclipse-temurin:21-jdk \
  sh -c 'cp -a $JAVA_HOME /tc/jdk21'

# Android SDK: install under the JDK you just placed, then accept the licences.
docker run --rm -v quack-toolchains:/tc -e JAVA_HOME=/tc/jdk21 debian:bookworm-slim sh -c '
  export PATH=/tc/jdk21/bin:$PATH
  /tc/android-sdk/cmdline-tools/latest/bin/sdkmanager --sdk_root=/tc/android-sdk \
    "platform-tools" "platforms;android-35" "build-tools;35.0.0"
  yes | /tc/android-sdk/cmdline-tools/latest/bin/sdkmanager --sdk_root=/tc/android-sdk --licenses'

# quack runs as uid 65532 and must be able to READ everything here.
docker run --rm -v quack-toolchains:/tc debian:bookworm-slim chown -R 65532:65532 /tc
```

Mount it read-only into the container (`quack-toolchains:/toolchains`) and declare it `external: true` in compose, so a `down -v` can never wipe several GB of SDK.

Verify as the real uid before going further — a toolchain root owned by the wrong user fails in confusing ways later:

```bash
docker run --rm --user 65532 -v quack-toolchains:/toolchains debian:bookworm-slim \
  /toolchains/jdk21/bin/java -version
```

> **Gotcha:** piping `docker run` into `head` or `tail` can SIGPIPE the image pull and silently abort the copy, leaving a half-populated volume. Redirect to a file instead.

## Java and Android

```yaml
workspace:
  exec_path:
    - /toolchains/jdk21/bin
    - /toolchains/android-sdk/platform-tools
    - /toolchains/android-sdk/cmdline-tools/latest/bin
  env:
    JAVA_HOME: /toolchains/jdk21
    ANDROID_HOME: /toolchains/android-sdk
    ANDROID_SDK_ROOT: /toolchains/android-sdk
    # JVM tooling ignores $HOME - see below.
    GRADLE_USER_HOME: /workspace/local/.quack-home/.gradle
    ANDROID_USER_HOME: /workspace/local/.quack-home/.android
    ANDROID_PREFS_ROOT: /workspace/local/.quack-home
    GRADLE_OPTS: "-Dorg.gradle.daemon=false"
  check_commands: ["go build", "go vet", "go test", "npm run", "npm test", "npx tsc", "make", "./gradlew"]
```

**The non-obvious part: JVM tools ignore `$HOME`.** quack gives every child an isolated, writable home (the jail's per-user `.quack-home`) via `HOME`, and that is enough for `npm`, `pip`, and friends. It is *not* enough for anything on the JVM: Java derives `user.home` from the **passwd entry**, not the environment, so every JVM tool resolves it to the image's `/home/nonroot` — which does not exist and is not writable. Each tool therefore needs its own explicit variable. Without them the failures are real but unrecognisable:

| Missing | Failure |
| --- | --- |
| `GRADLE_USER_HOME` | `Could not create parent directory for lock file /home/nonroot/.gradle/...` |
| `ANDROID_USER_HOME` / `ANDROID_PREFS_ROOT` | `Failed to create service AndroidLocationsBuildService` while applying the Android plugin |

`GRADLE_OPTS` disabling the daemon is deliberate: each gate check is a fresh short-lived child, so a surviving daemon is just memory held on a box that is probably also serving models.

**Multiple JDKs.** `workspace.env` is deployment-wide, so there is exactly one `JAVA_HOME` — the gate's `workspaceCaps` is built once and shared by every node. Point `JAVA_HOME` at the floor your build plugin requires (AGP 8.x needs 17+) and let Gradle select per project via its own toolchain support:

```yaml
    GRADLE_OPTS: "-Dorg.gradle.daemon=false -Dorg.gradle.java.installations.paths=/toolchains/jdk17,/toolchains/jdk21,/toolchains/jdk25"
```

That works from Gradle 6.7 onward. Below that, `JAVA_HOME` is the only lever.

## Go

The image already ships Go, so this is only needed to pin a *different* version than the image's:

```yaml
workspace:
  exec_path:
    - /toolchains/go1.25.0/bin
    - /toolchains/go1.25.0        # GOROOT itself: bind the whole prefix, not just bin/
  env:
    GOROOT: /toolchains/go1.25.0
```

List the SDK root as well as its `bin/`. Under `sandbox: bwrap`, `exec_path` entries are what get bind-mounted into the child's namespace, so binding only `bin/` leaves the compiler present but its `src/` and `pkg/` missing — the toolchain then fails with `cannot find GOROOT` rather than "not found", which reads like a corrupt install.

`GOPATH`/`GOCACHE` need no special handling: they default under `$HOME`, and unlike the JVM, Go honours it.

## Verifying

Config that loads is not config that works. Run the project's own test command in the container, with the same environment a gate check gets:

```bash
docker exec -e JAVA_HOME=/toolchains/jdk21 \
            -e ANDROID_HOME=/toolchains/android-sdk \
            -e GRADLE_USER_HOME=/workspace/local/.quack-home/.gradle \
            -e ANDROID_USER_HOME=/workspace/local/.quack-home/.android \
            -e ANDROID_PREFS_ROOT=/workspace/local/.quack-home \
            quack sh -c 'export PATH=/toolchains/jdk21/bin:$PATH
              cd /workspace/local/<chat-id>/quack-shared-repo && ./gradlew testDebugUnitTest'
```

Check the config parses too — `quack server validate <path>` loads it without starting the server. Note that it will *not* catch a misspelled or renamed key: unknown YAML keys are currently ignored rather than rejected ([#560](https://github.com/fagerbergj/quack/issues/560)), so a stale key silently disables whatever it configured.

Finally, watch `quack.gate.checks.skipped` after enabling a new toolchain. A `no_checks_derived` reason means the gate found the repo but recognised nothing to run — usually the build command is missing from `check_commands`. Checks are derived for **implementer nodes only**, so review and explorer nodes reporting `not_configured` is expected, not a misconfiguration.
