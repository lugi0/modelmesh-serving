#!/usr/bin/env bash
# Copyright 2021 IBM Corporation
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.#

set -euo pipefail
#shopt -s inherit_errexit

CONTEXT_DIR=devbuild
IMAGE_NAME=kserve/modelmesh-controller-develop
DEV_DEPS="$0 Dockerfile.develop go.mod go.sum .pre-commit-config.yaml"

# command is shasum on osx
SHASUM=sha1sum
if ! which $SHASUM; then SHASUM=shasum; fi
DEV_IMG_TAG=$(cat $(ls ${DEV_DEPS}) | ${SHASUM} | head -c 16)

FULL_IMAGE_NAME="${IMAGE_NAME}:${DEV_IMG_TAG}"
echo "Pulling dev image ${FULL_IMAGE_NAME}..."
if podman pull -q ${FULL_IMAGE_NAME}; then
    echo "Successfully pulled dev image ${FULL_IMAGE_NAME}"
else
  mkdir -p $CONTEXT_DIR
  cp ${DEV_DEPS} ${CONTEXT_DIR}
  echo "Building dev image ${FULL_IMAGE_NAME}"
  podman build -f ${CONTEXT_DIR}/Dockerfile.develop -t ${FULL_IMAGE_NAME} ${CONTEXT_DIR}
fi
echo -n "${FULL_IMAGE_NAME}" > .develop_image_name

NUM_LAYERS=$(podman images -q "${FULL_IMAGE_NAME}" | xargs podman history | egrep -v "^IMAGE" | wc -l | tr -d ' ')
echo "Image ${FULL_IMAGE_NAME} has ${NUM_LAYERS} layers"

echo "Tagging dev image ${FULL_IMAGE_NAME} as latest"
podman tag ${FULL_IMAGE_NAME} "${IMAGE_NAME}:latest"
