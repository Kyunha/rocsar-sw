{ pkgs ? import (fetchTarball "https://github.com/NixOS/nixpkgs/archive/refs/heads/nixos-unstable.tar.gz") {} }:

let
  python = pkgs.python3.withPackages (ps: with ps; [
    pyzmq
    protobuf
    setuptools
    pip
    pytest
    pytest-cov
    mypy
    ruff
  ]);
in
pkgs.mkShell {
  packages = with pkgs; [
    python
    buf
    go
    git
    fish
  ];

  # -----------------------------------------------------------------------
  # Ground Station (cmd/gs) toolchain. See GUI_ARCHITECTURE.md section 12.
  #
  # nativeBuildInputs, NOT packages, and that distinction is the whole reason
  # this list is separate. `packages` only prepends to PATH; `nativeBuildInputs`
  # additionally runs each derivation's setup hooks, and pkg-config's setup hook
  # is what puts a library's .pc files on the search path. With webkitgtk in
  # `packages`, `pkg-config --modversion webkit2gtk-4.1` reports "not found"
  # while the library is sitting in the store fully downloaded -- a failure that
  # reads like a missing dependency and is really a misplaced list.
  #
  # Every attribute name here was checked against nixpkgs rather than assumed.
  # The two that are easy to get wrong:
  #
  #   - it is `webkitgtk_4_1`, not `webkit2gtk_4_1`. The latter does not exist,
  #     and a shell naming it fails to evaluate with an unhelpful
  #     "attribute not found" instead of a build error.
  #   - `wails` is the CLI and is currently 2.16.0, which is the version
  #     GUI_ARCHITECTURE.md pins. There is no `wails-cli` attribute; asking for
  #     one fails the same unhelpful way.
  #
  # `zeromq` is here for cgo, not for a human: go-zeromq/zmq4 links against
  # libzmq, so the OBC and the GS client both need it present at compile time.
  # The attribute is `zeromq` and the .pc file it installs calls itself
  # `libzmq`; the two names differ.
  nativeBuildInputs = with pkgs; [
    wails
    nodejs
    pkg-config
    webkitgtk_4_1
    zeromq
  ];

  shellHook = ''
    echo "ROCSAR GroundStation -- development shell"
    python --version

    # NOTE: the `webkit2_41` build tag does NOT belong here.
    #
    # It was tried here first, in GOFLAGS, and it does not work. `wails build`
    # and `wails dev` assemble their own tag list -- output type, mode,
    # obfuscation, plus whatever you pass to `-tags` -- and always pass it as an
    # explicit `-tags` on the `go build` command line. A command-line -tags
    # overrides GOFLAGS, so the tag was silently dropped and the build died in
    # cgo looking for `webkit2gtk-4.0`, a pkg-config module that does not exist
    # on any current distribution. `wails build` prints its compiled tag list,
    # and it read `Tags | []`.
    #
    # The tag lives in cmd/gs/wails.json as "build:tags", which is the one place
    # both `build` and `dev` read it from. See GUI_ARCHITECTURE.md section 12.
    #
    # It is also not needed for a bare `go build ./...`: the webview packages
    # hang off the `desktop` tag, which only wails sets, so without it they are
    # never reached and cgo is never invoked. That is why the repo-wide
    # compile check works in a plain shell with no webkit at all.

    # A missing prerequisite is worth saying out loud. `wails build` fails
    # deep inside cgo otherwise, with a message about pkg-config.
    if ! pkg-config --exists webkit2gtk-4.1; then
      echo "WARNING: webkit2gtk-4.1 not found; the Ground Station will not link." >&2
    fi
    if ! pkg-config --exists libzmq; then
      echo "WARNING: libzmq not found; go-zeromq/zmq4 will not link." >&2
    fi

    echo "wails $(wails version 2>/dev/null | tail -1), node $(node --version)"

    if [[ $- == *i* ]]; then
      exec ${pkgs.fish}/bin/fish
    fi
  '';
}