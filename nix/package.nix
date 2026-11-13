{
  lib,
  buildGo127Module,
  rev,
}:

buildGo127Module {
  pname = "towa";
  version = rev;

  # Only what the binary is built from, so that a change to the docs or the
  # tests of another module does not rebuild it.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../src
    ];
  };

  # The hash of the modules go.sum names. After a dependency changes, set it
  # to lib.fakeHash, build, and copy the hash the error prints.
  vendorHash = "sha256-I3FpjjkaAAI9mmAn9dhw1xrgQgjjo2DctHLHKpA5SSg=";

  subPackages = [ "cmd/towa" ];

  # The release binary is static, as CI builds it.
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X github.com/gfx-labs/towa/src/cli.buildVersion=${rev}"
  ];

  # The tests need the network, a shell and recorded logins. CI runs them.
  doCheck = false;

  # Nothing is wrapped: the binary is one static file. The model runs bash,
  # rg and find through the shell tool, from the PATH towa is started with.

  meta = {
    description = "A coding agent that speaks the Agent Client Protocol";
    homepage = "https://github.com/gfx-labs/towa";
    license = lib.licenses.asl20;
    mainProgram = "towa";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}
