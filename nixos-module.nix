{ self }:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.prixdugaz;
in
{
  options.services.prixdugaz = {
    enable = lib.mkEnableOption "Prix du gaz Quebec gas price map";

    port = lib.mkOption {
      type = lib.types.port;
      default = 8080;
      description = "Port the Prix du gaz web server listens on.";
    };

    dataDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/prixdugaz";
      description = "Directory where the SQLite database is stored.";
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Whether to open the firewall for the Prix du gaz web server port.";
    };

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.system}.default;
      defaultText = lib.literalExpression "self.packages.\${pkgs.system}.default";
      description = "The Prix du gaz package to use.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.prixdugaz = {
      description = "Prix du gaz Quebec - Gas price map";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      environment = {
        PORT          = toString cfg.port;
        PRIXDUGAZ_DB  = "${cfg.dataDir}/prixdugaz.db";
      };

      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        Restart = "on-failure";
        RestartSec = 5;

        DynamicUser = true;
        StateDirectory = "prixdugaz";
        StateDirectoryMode = "0750";

        # Hardening
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictSUIDSGID = true;
        RestrictNamespaces = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        RestrictRealtime = true;
        SystemCallFilter = [ "@system-service" "~@privileged" ];
      };
    };

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ cfg.port ];
  };
}
