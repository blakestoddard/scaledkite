FROM buildkite/agent:3.17.0-ubuntu

RUN apt-get update && apt-get -y install gnupg curl ruby

RUN gem install bundler

# Install awscli
RUN apt-get update && \
    apt-get install -y \
        python3 \
        python3-pip \
        python3-setuptools \
        groff \
        less \
    && pip3 install --upgrade pip \
    && apt-get clean
RUN pip3 --no-cache-dir install --upgrade awscli

# install docker-browse so we can check an image exists before deploy
RUN curl -sL https://deb.nodesource.com/setup_8.x | bash -
RUN apt-get install -y nodejs
RUN npm install -g docker-browse

# install kubectl for pre-exit hook
RUN curl -LO https://storage.googleapis.com/kubernetes-release/release/v1.13.12/bin/linux/amd64/kubectl && \
    chmod +x ./kubectl && \
    mv ./kubectl /usr/local/bin/kubectl


# Bump docker-compose due to https://github.com/docker/compose/issues/5950
RUN curl -L "https://github.com/docker/compose/releases/download/1.23.0-rc2/docker-compose-$(uname -s)-$(uname -m)" -o /usr/local/bin/docker-compose

ADD hooks/environment /buildkite/hooks/environment
ADD hooks/pre-exit /buildkite/hooks/pre-exit