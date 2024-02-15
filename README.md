# awi-grpc-catalyst-sdwan

This is the AWI GRPC plugin for Cisco catalyst SDWAN. This plugin can run independently on your laptop or run within your Kubernetes cluster as part of AWI Operator.

## Running

Check out the code and from your CLI pass Catalyst SDWAN auth parameters in command line, or as environment variables.

VMANAGE_USERNAME=<your_username> VMANAGE_PASSWORD=<your_password> make run

Use config.yaml in the root directory for configuration .

### Configuration

Below is the sample configuration in YAML:

```yaml
controllers:
  sdwan:
    controller_connection_retries: 200 # How many times to try before giving up
    name: cisco-sdwan.                 # Controller name
    retries_interval: 5s               # Retry frequency
    secure_connection: false           # Set it to false for self signed certificates.
    url: https://<<catalyst-sdwan-controller-host-name>>:<port>/ # SDWAN controller URL
    vendor: cisco
globals:
  #hostname: 127.0.0.1                 # if empty listen on all available IP addresses
  port: 50051                          # GRPC port number
  controller_connection_retries: 100
  db_name: awi.db                      # Local database port for storing state.
  #log_file: awi.log                   # Log file name
  log_level: DEBUG                     # Log level
  retries_interval: 2s
  secure_connection: false
  #kube_config_file: kubeconfig
```

## Docker instructions

### Building and pushing image

To build your image:

```sh
make docker-build IMG=<your-repo>/<name>
```

To push it to your repository:

```sh
make docker-push IMG=<your-repo>/<name>
```

> ℹ️ Info: You can also do both steps at once with
> `make docker-build docker-push IMG=<your-repo>/<name>`

### Running docker image

The awi-grpc-catalyst-sdwan accepts following files:

* /root/config/config.yaml - the configuration file
* /root/.aws/credentials - the credentials for AWS
* /app/gcp-key/gcp-key.json - the credentials for GCP
* /root/.kube/config - configuration and credentials for k8s cluster

In order tp configure and gain access for different providers for awi-grpc-catalyst-sdwan
one need to mount these files while starting container.

To configure VMANAGE Credentials, specify the following environment variables for the container:

* VMANAGE_USERNAME
* VMANAGE_PASSWORD

## Contributing

Thank you for interest in contributing! Please refer to our
[contributing guide](CONTRIBUTING.md).

## License

awi-grpc-catalyst-sdwan is released under the Apache 2.0 license. See
[LICENSE](./LICENSE).

awi-grpc-catalyst-sdwan is also made possible thanks to
[third party open source projects](NOTICE).
