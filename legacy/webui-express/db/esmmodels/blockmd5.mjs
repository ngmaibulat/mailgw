import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class BlockMD5 extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

BlockMD5.init(
    {
        md5: DataTypes.STRING,
        comment: DataTypes.STRING,
    },
    {
        sequelize,
        modelName: "BlockMD5",
    }
);

export default BlockMD5;
