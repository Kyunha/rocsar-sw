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

  # libzmq for cgo. Nixpkgs ships it under `zeromq`; the pkg-config file it
  # installs calls itself `libzmq`, so the attribute name and the library name
  # differ. Resolved here rather than interpolated into the shellHook, because
  # a Nix interpolation buried in the middle of an indented bash string is
  # exactly the kind of thing an editor's Nix parser gets wrong.
  libzmqPkgConfigPath = "${pkgs.zeromq}/lib/pkgconfig";
in
pkgs.mkShell {
  packages = with pkgs; [
    python
    buf
    go
    git
    fish

    # ---------------------------------------------------------------------
    # Ground Station (cmd/gs) toolchain. See GUI_ARCHITECTURE.md section 12.
    #
    # Every name here was checked against nixpkgs rather than assumed. The
    # two that are easy to get wrong:
    #
    #   - the WebKitGTK attribute is `webkitgtk_4_1`, NOT `webkit2gtk_4_1`.
    #     The latter does not exist and a shell that lists it fails to realise
    #     with an unhelpful "attribute not found" rather than a build error.
    #   - `wails` is the CLI and is currently 2.16.0, which is the version
    #     GUI_ARCHITECTURE.md pins. There is no `wails-cli` attribute; asking
    #     for one produces the same unhelpful failure.
    #
    # `zeromq` is here for cgo. go-zeromq/zmq4 links against libzmq, so both
    # the OBC and the GS client need it present at compile time -- a laptop
    # with no system libzmq cannot build either.
    wails
    nodejs
    pkg-config
    webkitgtk_4_1
    zeromq
  ];

  shellHook = ''
    echo "ROCSAR GroundStation -- development shell"
    python --version

    # Wails v2's Linux frontend has two implementations behind build tags:
    # the default is the WebKit2GTK 4.0 API, `webkit2_41` selects 4.1. Current
    # distributions -- and webkitgtk_4_1 -- ship only 4.1, so without the tag
    # cgo fails looking for `webkit2gtk-4.0` pkg-config modules that do not
    # exist. The failure names a package that was never going to be installed,
    # which is why this is set here rather than left to each build command.
    #
    # GOFLAGS rather than a `wails build -tags` alias: `wails build` and
    # `wails dev` both shell out to `go build`/`go run`, so this is the one
    # place that covers every path, including a bare `go build ./...`.
    # Set outright rather than appended to. `nix-shell` starts from a clean
    # environment, so there is no inherited GOFLAGS worth preserving, and the
    # append form needs a literal `${` that has to be escaped for Nix --
    # `''${` -- inside an already-quoted indented string. Not worth the
    # confusion. If you need extra flags, add them here, in one place.
    export GOFLAGS="-tags=webkit2_41"

    # See the note on libzmqPkgConfigPath above for why this is a plain
    # assignment and not an append: the shell is entered fresh from a clean
    # environment, so there is nothing to preserve and an append would only
    # risk a second, stale path shadowing the right one.
    export PKG_CONFIG_PATH="${libzmqPkgConfigPath}"

    echo "wails $(wails version 2>/dev/null | head -1), node $(node --version)"

    if [[ $- == *i* ]]; then
      exec ${pkgs.fish}/bin/fish
    fi
  '';
}