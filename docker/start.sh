#!/bin/sh

# directory setup
mkdir -p /data/ssh
mkdir -p /data/gogs/log
mkdir -p /data/gogs/data

#SSH setup
# Check if host keys are present, else create them
if ! test -f /data/ssh/ssh_host_rsa_key; then
    ssh-keygen -q -f /data/ssh/ssh_host_rsa_key -N '' -t rsa
fi

if ! test -f /data/ssh/ssh_host_dsa_key; then
    ssh-keygen -q -f /data/ssh/ssh_host_dsa_key -N '' -t dsa
fi

if ! test -f /data/ssh/ssh_host_ecdsa_key; then
    ssh-keygen -q -f /data/ssh/ssh_host_ecdsa_key -N '' -t ecdsa
fi

if ! test -f /data/ssh/ssh_host_ed25519_key; then
    ssh-keygen -q -f /data/ssh/ssh_host_ed25519_key -N '' -t ed25519
fi

if ! test -d ~git/.ssh; then
    mkdir -p ~git/.ssh
    chmod 700 ~git/.ssh
fi

#Gogs setup
if ! test -f ~git/.ssh/environment; then
    echo "GOGS_CUSTOM=${GOGS_CUSTOM}" > ~git/.ssh/environment
    chmod 600 ~git/.ssh/environment
fi

cd /app/gogs

# check authorized keys
if ! test -f /data/git/.ssh/authorized_keys; then
    echo "" > /data/git/.ssh/authorized_keys
fi
# check authorized keys
if ! test -d /data/.ssh; then
    mkdir -p /data/.ssh
fi

# Link volumed data with app data
ln -sf /data/gogs/log  ./log
ln -sf /data/gogs/data ./data
ln -s /data/git/.ssh/authorized_keys /data/.ssh/authorized_keys

# Backward Compatibility with Gogs Container v0.6.15
ln -sf /data/git /home/git

# check whether we nedd to reset userid
if [ ! -z "$GITUID" ]; then
    echo "change git user id to $GITUID"
    usermod -u "$GITUID" git
fi
if [ ! -z "$GITGID" ]; then
    echo "change git group id to $GITGID"
    groupmod -g "$GITGID" git
fi

chown -R git:git /data /app/gogs ~git/
chmod 0755 /data /data/gogs ~git/

# Set correct right to ssh keys
chown -R root:root /data/ssh/*
chmod 0700 /data/ssh
chmod 0600 /data/ssh/*
# Exec CMD or S6 by default if nothing present
supervisord -c /app/gogs/docker/supervisord.conf

