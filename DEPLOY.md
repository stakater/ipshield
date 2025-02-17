# Deployment

## Manual Deployment
For local development and testing, you can build and push the operator image directly to a Docker repository.

### Steps
1. Update `VERSION` and `DOCKER_REPO_BASE` variables in the `Makefile`.
2. Build the controller and bundle images:
   ```sh
   make manifests build docker-build docker-push
   make bundle bundle-build bundle-push
   ```
3. Add a version entry to `catalog/channels.yaml` following OLM upgrade specifications:
   ```yaml
   entries:
   - name: ipshield-operator.v0.0.1
   - name: ipshield-operator.v0.0.2 # This is the next version to be released
     skips:
      - ipshield-operator.v0.0.1
      ```
4. Render, build, and push the catalog index:
   ```sh
   make catalog-render catalog-build catalog-push
   ```

## CI/CD Deployment
For automated builds and releases via CI/CD pipelines, the version is bumped based on the latest release tag rather than the `Makefile` settings.

### Conditions for Catalog Release

A catalog release is triggered if:
- Changes are made to files in the `catalog` directory.
- A new version entry is specified in `catalog/channels.yaml,` indicating the upcoming release.
  ```yaml
  entries:
  - name: ipshield-operator.v0.0.1
  - name: ipshield-operator.v0.0.2 # This is the next version to be released
    skips:
    - ipshield-operator.v0.0.1
  ```

## Working with Entries in `catalog/channels.yaml`

The entries section in `catalog/channels.yaml` defines versioning information for OLM to manage upgrades. Properly structuring these entries is essential for ensuring smooth version handling and change application.

### Semantic Versioning

Strict adherence to semantic versioning is required. Proper versioning is critical for maintaining smooth upgrades and avoiding deployment issues with OLM.
- **Major version changes** indicate breaking changes (e.g., v1.0.0 → v2.0.0).
- **Minor version changes** introduce backward-compatible features (e.g., v0.1.0 → v0.2.0).
- **Patch version changes** fix bugs or make minor improvements (e.g., v0.0.1 → v0.0.2).

### Entry Structure

Each release of has an entry in the entries section of `catalog/channels.yaml`. The following structure is used in the repository for entries:

#### Non-breaking API Changes (Use skips)
For non-breaking API changes (such as bug fixes or new features), the skips field is used to specify previous versions that are skipped during upgrades.

Example:

```yaml
entries:
- name: ipshield-operator.v0.0.3
  skips:
   - ipshield-operator.v0.0.1
   - ipshield-operator.v0.0.2
```
#### Breaking API Changes (Use replaces)
For breaking API changes, the replaces field is used to specify which previous version is being replaced. This indicates to OLM that the older version should be upgraded to the new version.
Example:

```yaml
entries:
- name: ipshield-operator.v0.0.3
  replaces: ipshield-operator.v0.0.1
```

## Contributing
1. Add the next version entry in `catalog/channels.yaml`:
   ```yaml
   entries:
      - name: ipshield-operator.v{{CURRENT}}
      - name: ipshield-operator.v{{NEXT}}
        skips:
          - ipshield-operator.v0.0.1
   ```
2. Create a PR and run tests.
3. Merging the PR will trigger the build and push the releases.

## More Information
Refer to the OpenShift documentation for managing custom catalogs:
[OLM Managing Custom Catalogs](https://docs.openshift.com/container-platform/latest/operators/admin/olm-managing-custom-catalogs.html)

