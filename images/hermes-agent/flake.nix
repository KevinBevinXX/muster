{
  # The `hermes` agent kind's image: Hermes at ONE pinned upstream commit, with
  # muster's hermesd in front of it (docs/design/hermes.md, "Image").
  #
  #     nix build ./images/hermes-agent#image      # a stream script: `$(readlink result) | docker load`
  #     go run ./images/hermes-agent/smoke -image hermes-agent:<tag>
  #
  # 🔴 THE PIN IS A SHA, NEVER A TAG. Canary tags are deleted 14 days after they
  # are cut (P:scripts/release.py:575-580), and committed source always reports
  # version 0.0.0, so the sha is the only name that keeps meaning the same tree.
  # Move it only by tree (the design's "Versions" section), together with the
  # mirroring hosts' pin.
  #
  # 🔴 A SEPARATE FLAKE, so upstream Hermes's inputs (its own nixpkgs, uv2nix,
  # home-manager, …) live in THIS lock, not in muster's: nothing that builds
  # muster fetches them. hermesd itself comes from muster's flake through the
  # relative `path:` input, built there from the one `goVendorHash`.
  description = "muster's hermes agent image: Hermes at a pinned sha behind hermesd";

  inputs = {
    hermes-agent.url = "github:NousResearch/hermes-agent/5fc490e2b34123212d73ef316086f939567a725a";
    nixpkgs.follows = "hermes-agent/nixpkgs";
    muster.url = "path:../..";
  };

  outputs = { self, hermes-agent, nixpkgs, muster }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      pin = "5fc490e2b34123212d73ef316086f939567a725a";

      # 🔴 UPSTREAM `minimal` WITH ffmpeg OVERRIDDEN OUT. ffmpeg (even
      # ffmpeg-headless) drags in a C toolchain: upstream `minimal` is a 3,278 MiB
      # closure, this one 1,149 MiB (S8). ffmpeg serves audio and voice, which a
      # pod does not offer (the rendered config disables the tts toolset).
      # `P:nix/hermes-agent.nix` takes `ffmpeg` as a callPackage argument.
      hermes = hermes-agent.packages.${system}.minimal.override { ffmpeg = pkgs.emptyDirectory; };
      hermesd = muster.packages.${system}.hermesd;

      # 🔴 /etc/passwd AND /etc/group FOR uid 1000. Without an entry Hermes's
      # real-home lookup falls back to a scratch directory (S8); the home is the
      # agent shell's, $HERMES_HOME/home, which hermesd's seed creates.
      passwd = pkgs.writeTextDir "etc/passwd" ''
        root:x:0:0:root:/root:/bin/sh
        hermes:x:1000:1000:hermes:/data/hermes/home:/bin/sh
        nobody:x:65534:65534:nobody:/:/bin/sh
      '';
      group = pkgs.writeTextDir "etc/group" ''
        root:x:0:
        hermes:x:1000:
        nobody:x:65534:
      '';
      # The pin, readable inside the image.
      pinFile = pkgs.writeTextDir "etc/hermesd/hermes-pin" "${pin}\n";
    in
    {
      packages.${system} = {
        inherit hermes hermesd;

        # A stream script (`$(readlink result) | docker load`): the image is
        # assembled as it is loaded, never written to the store whole.
        image = pkgs.dockerTools.streamLayeredImage {
          name = "hermes-agent";
          # The pin names the tag; hermesd's own revision is the deploy's to record (it
          # pushes and pins the image by digest, MUSTER_AGENT_HERMES_IMAGE).
          tag = "hermes-${builtins.substring 0 8 pin}";
          contents = [
            hermesd hermes passwd group pinFile
            # 🔴 tini IS PID 1, NOT hermesd. The gateway's terminal tool starts
            # processes the gateway can leave behind when it is killed; they are
            # reparented to PID 1, which must reap them. tini does, and forwards
            # SIGTERM to hermesd, which forwards it to the gateway.
            pkgs.tini
            pkgs.bashInteractive pkgs.coreutils pkgs.findutils pkgs.gnugrep pkgs.gnused pkgs.gnutar pkgs.gzip
            pkgs.git pkgs.curl pkgs.cacert pkgs.ripgrep pkgs.procps
          ];
          # 🔴 /data OWNED BY uid 1000: a container runtime creates a missing
          # mount point as root, and a fresh volume initialised from the image
          # takes this ownership.
          fakeRootCommands = ''
            mkdir -p ./data ./tmp
            chmod 1777 ./tmp
            chown -R 1000:1000 ./data
          '';
          enableFakechroot = false;
          config = {
            Entrypoint = [ "${pkgs.tini}/bin/tini" "--" "${pkgs.lib.getExe hermesd}" ];
            Cmd = [ "serve" ];
            User = "1000:1000";
            Env = [
              "PATH=/bin"
              "HERMES_HOME=/data/hermes"
              "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
              "GIT_SSL_CAINFO=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
            ];
            ExposedPorts = { "18789/tcp" = { }; };
            Labels = {
              "org.opencontainers.image.source" = "https://github.com/ZacxDev/muster";
              "dev.muster.hermes.pin" = pin;
            };
          };
        };
      };
    };
}
