{ pkgs ? import (fetchTarball "https://github.com/NixOS/nixpkgs/archive/refs/heads/nixos-unstable.tar.gz") {} }:

pkgs.mkShell {
  packages = with pkgs; [
    python3
    python3Packages.pytest
    buf
    go
    git
    fish
  ];

  nativeBuildInputs = with pkgs; [
    nodejs
    pkg-config
    gcc
  ];

  buildInputs = with pkgs; [
    zeromq
  ];

  shellHook = ''
    export CGO_ENABLED=1

    echo "ROCSAR Ground Station -- development shell"
    python3 --version

    if ! pkg-config --exists libzmq; then
      echo "WARNING: libzmq not found; go-zeromq/zmq4 will not link." >&2
    fi

    echo "node $(node --version)"

    if [[ $- == *i* ]]; then
      exec ${pkgs.fish}/bin/fish
    fi
  '';
}
