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

  shellHook = ''
    echo "ROCSAR GroundStation -- development shell"
    python --version

    if [[ $- == *i* ]]; then
      exec ${pkgs.fish}/bin/fish
    fi
  '';
}
