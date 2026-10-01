# envconsul configuration for Oberth release steps.
#
# envconsul authenticates to OpenBao/Vault using the Kubernetes auth method
# and injects secret values into the child process environment. The
# credentialed ServiceAccount token is mounted by Oberth when the pipeline
# declares approved secret-store paths.
#
# VAULT_ADDR and VAULT_CACERT are injected by Oberth into every credentialed
# container.

vault {
  # Kubernetes auth login using the projected ServiceAccount token.
  auth {
    enabled = true
    type    = "kubernetes"
    config {
      # OBERTH_VAULT_ROLE is injected by Oberth alongside VAULT_ADDR.
      role = "${OBERTH_VAULT_ROLE}"
    }
  }

  ssl {
    enabled = true
    verify  = true
  }
}

# Map secret paths to environment variables. Adjust paths to match the
# oberth.ci/secret-paths annotation on the release Workflow.
secret {
  path = "myproject/data/release/cosign-key"

  key {
    name   = "cosign.key"
    format = "COSIGN_KEY"
  }
}

secret {
  path = "myproject/data/release/registry-token"

  key {
    name   = "token"
    format = "REGISTRY_TOKEN"
  }
}
