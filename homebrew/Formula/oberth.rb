class Oberth < Formula
  desc "Single-node Git-over-SSH CI service for Kubernetes with repository-owned Go pipelines"
  homepage "https://oberth.ci"
  version "0.9.99"
  license "Proprietary"

  on_macos do
    on_intel do
      url "https://releases.cloudtaser.io/oberth/v0.9.99/oberth-darwin-amd64"
      sha256 "8dee725c0e06e1c630fa67bd64e7dd854e12177b7f0f0129d535b44b0a876657"
    end
    on_arm do
      url "https://releases.cloudtaser.io/oberth/v0.9.99/oberth-darwin-arm64"
      sha256 "3c287eb4b676a0517f5904b65cefd65832c858d01f9a7b9b9d49ceee0e82f8a6"
    end
  end

  on_linux do
    on_intel do
      url "https://releases.cloudtaser.io/oberth/v0.9.99/oberth-linux-amd64"
      sha256 "11424b54e2aa627fc9ed5acdf54db3a36749d72bdb7595adb0a725dbf2396eec"
    end
    on_arm do
      url "https://releases.cloudtaser.io/oberth/v0.9.99/oberth-linux-arm64"
      sha256 "765d9922b71980045dab7ba4c43ea5123f3d5815283bd7df088fc2edf75c95c1"
    end
  end

  def install
    binary = stable.url.split("/").last
    bin.install binary => "oberth"
  end

  test do
    system bin/"oberth", "version"
  end
end
