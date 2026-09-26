{
  description = "kei-plugin-agent — kei 的「LLM 人格代理」插件（群聊里按人格预设偶尔插话）";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      pkgsFor = system: nixpkgs.legacyPackages.${system};

      devTools =
        pkgs: with pkgs; [
          go # 编译器 / go test / go vet
          gopls # LSP
          gotools # goimports 等辅助工具
          golangci-lint # 静态检查
          delve # 调试器 dlv
          jq # 解析 mock 适配器 / LLM 桩的 JSON 载荷
          curl # 手工注入事件、观察发送
          git
        ];
    in
    {
      # 本仓库是库/插件，构建依赖同级 kei 检出（go.mod 的 replace ../kei），
      # nix 沙箱内没有该目录，因此不提供 packages/checks：
      # 质量门在 devShell 内直接执行 go build / go vet / go test。
      devShells = forAllSystems (system: {
        default = (pkgsFor system).mkShell {
          name = "kei-plugin-agent";

          packages = devTools (pkgsFor system);

          # 只使用 devShell 提供的 Go，禁止 go 自动下载其它 toolchain。
          env.GOTOOLCHAIN = "local";

          shellHook = ''
            echo "kei-plugin-agent dev shell · $(go version)"
            echo "常用命令: go build ./... | go vet ./... | go test ./... | go test -race ./... | golangci-lint run"
            if [ ! -d ../kei ]; then
              echo "提示: 本插件依赖同级 kei 检出（go.mod replace ../kei），请先 git clone https://github.com/RandomLemon/kei ../kei"
            fi
          '';
        };
      });

      # nixfmt-tree 让 `nix fmt` 在无参数时也能格式化目录树下的所有 .nix 文件。
      formatter = forAllSystems (system: (pkgsFor system).nixfmt-tree);
    };
}
