#### Update release version in Makefile
``VERSION ?= {{VERSION}}`` for example 0.0.5

#### Build & push operator image
``make manifests build docker-build docker-push``

### Build & push Bundle image
``make bundle bundle-build bundle-push``

### Add Bundle to catalog/index.yaml
``opm render docker.io/stakaterdockerhubpullroot/{{NAME}}-operator-bundle:v{{VERSION}} --output=yaml >> 
catalog/index.yaml``

### Adjust OLM entries & upgrade path
1. Skipping
    ```
    entries:
      - name: {{NAME}}-operator.v0.0.5
        skips:
          - {{NAME}}-operator.v0.0.1
          - {{NAME}}-operator.v0.0.2
          - ....
    ```
2. Upgrading
    ```
    entries:
      - name: {{NAME}}-operator.v0.0.5
        replaces: {{NAME}}-operator.v0.0.4
    ```

#### More information
https://docs.openshift.com/container-platform/4.17/operators/admin/olm-managing-custom-catalogs.html
