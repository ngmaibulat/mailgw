export os=`uname`

if [[ "$os" == "Darwin" ]]; then
    alias tar=gtar
fi

shopt -s expand_aliases

tar
