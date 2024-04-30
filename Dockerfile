# Copyright (c) 2023 Cisco Systems, Inc. and its affiliates
# All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http:www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.22-alpine3.18 AS builder

ARG SSH_PRIVATE_KEY

RUN mkdir -p /root/go/src/github.com/awi-grpc-catalyst-sdwan

WORKDIR /root/go/src/github.com/awi-grpc-catalyst-sdwan
COPY . .

RUN apk add make

RUN make build

# Second stage: create the runtime image
FROM alpine:3.18.4

WORKDIR /root
COPY --from=builder /root/go/src/github.com/awi-grpc-catalyst-sdwan/bin/awi-grpc-catalyst-sdwan .

# As k8s mounting makes it hard to create a file from a config map
# within the directory with already present files, we create a symlink
# to point to a new empty directory where actual config.yaml will be
# mounted.
RUN ln -s /root/config/config.yaml /root/config.yaml

CMD ["/root/awi-grpc-catalyst-sdwan"]

