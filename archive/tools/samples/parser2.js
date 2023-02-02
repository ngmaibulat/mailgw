const fs = require("fs");
const AttachChecker = require("../../plugins/AttachChecker");

const url = "http://localhost:3000/filter/md5";
const tmpfile = "/tmp/981B1730-DA9D-4820-9698-C3D43382F64B.1.tmp";
let inp = fs.createReadStream(tmpfile);

const checker = new AttachChecker(url);
checker.check(inp).then((res) => console.log(res));
